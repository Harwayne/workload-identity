package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"

	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/iam/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const (
	wiGSAAnnotation = "iam.gke.io/gcp-service-account"
)

var (
	serverFlag = flag.String("server", "",
		"The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	kubeconfigFlag = flag.String("kubeconfig", os.Getenv("KUBECONFIG"),
		"Path to a kubeconfig. Only required if out-of-cluster.")
)

var (
	ksaFlag     = flag.String("ksa", "", "KSA name")
	nsFlag      = flag.String("ns", "default", "Pod Namespace")
	podFlag     = flag.String("pod", "", "Pod name")
	projectFlag = flag.String("project", "", "Project ID")

	clusterProjectFlag  = flag.String("clusterProject", "", "Cluster Project")
	clusterLocationFlag = flag.String("clusterLocation", "", "Cluster Location")
	clusterNameFlag     = flag.String("clusterName", "", "Cluster Name")
)

var (
	ksaRoles = map[string]struct{}{
		"roles/iam.workloadIdentityUser":       {},
		"roles/iam.serviceAccountTokenCreator": {},
		"roles/editor":                         {},
		"roles/owner":                          {},
	}
)

func main() {
	flag.Parse()

	prefix := ""
	pod := *podFlag
	ksa := *ksaFlag

	if (ksa != "") == (pod != "") {
		log.Fatal("Exactly one of --ksa and --pod must be specified.")
	}

	ctx := context.Background()

	cfg, err := GetRESTConfig(*serverFlag, *kubeconfigFlag)
	if err != nil {
		log.Fatal("Error building kubeconfig: ", err)
	}

	client := kubernetes.NewForConfigOrDie(cfg)

	if pod != "" {
		prefix = fmt.Sprintf("Pod %q uses ", pod)
		var err error
		ksa, err = getPodKSA(ctx, client, *nsFlag, pod)
		if err != nil {
			log.Fatalf("Error getting the Pod's KSA: %v", err)
		}
	}

	gsa, err := getKSAsWIAnotation(ctx, client, *nsFlag, ksa)
	if err != nil {
		log.Fatalf("Error getting the KSA's WI annotation: %v", err)
	}

	clusterProject, clusterLocation, clusterName := "", "", ""
	if p, l, n, err := getClusterFromKubeconfig(); err == nil {
		clusterProject = p
		clusterLocation = l
		clusterName = n
	} else {
		clusterProject = *clusterProjectFlag
		clusterLocation = *clusterLocationFlag
		clusterName = *clusterNameFlag
	}

	wiPool, err := getWIPool(ctx, getClusterAPIName(clusterProject, clusterLocation, clusterName))
	if err != nil {
		log.Fatalf("Error getting WI Pool: %v", err)
	}
	if hasAccess, err := ksaHasAccessToGSA(ctx, wiPool, *nsFlag, ksa, gsa); err != nil {
		log.Fatalf("Error checking the KSAs access on the GSA: %v", err)
	} else if !hasAccess {
		log.Fatalf("%sKSA %q, which links to GSA %q, but that GSA does not grant access to the KSA",
			prefix, ksa, gsa)
	}
	project, err := determineProject(*projectFlag)
	if err != nil {
		log.Fatalf("Error getting project: %w", err)
	}
	roles, err := getGSAsRolesOnProject(ctx, project, gsa)
	if err != nil {
		log.Fatalf("Error getting the GSA %q's roles on project %q: %v", gsa, project, err)
	}

	fmt.Printf("%sKSA %q, which links to GSA %q, whose roles on the project %q are %v\n",
		prefix, ksa, gsa, project, roles)
}

func getPodKSA(ctx context.Context, client kubernetes.Interface, ns, podName string) (string, error) {
	pod, err := client.CoreV1().Pods(ns).Get(ctx, podName, v1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Spec.ServiceAccountName, nil
}

func getKSAsWIAnotation(ctx context.Context, client kubernetes.Interface, ns, ksaName string) (string, error) {
	ksa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, ksaName, v1.GetOptions{})
	if err != nil {
		return "", err
	}
	if gsa, present := ksa.Annotations[wiGSAAnnotation]; !present {
		return "", fmt.Errorf("ksa does not have the WI annotation, %q", wiGSAAnnotation)
	} else {
		return gsa, nil
	}
}

func ksaHasAccessToGSA(ctx context.Context, wiPool, ns, ksaName, gsaEmail string) (bool, error) {
	iamSVC, err := iam.NewService(ctx)
	if err != nil {
		return false, fmt.Errorf("creating IAM.Service: %w", err)
	}
	saSVC := iam.NewProjectsServiceAccountsService(iamSVC)
	gsaAPIResource := getGSAAPIResource(gsaEmail)
	gsaPolicy, err := saSVC.GetIamPolicy(gsaAPIResource).Do()
	if err != nil {
		return false, fmt.Errorf("getting GSA %q IAMPolicy: %w", gsaAPIResource, err)
	}
	ksaMember := ksaIAMPolicyMember(wiPool, ns, ksaName)
	for _, binding := range gsaPolicy.Bindings {
		for _, member := range binding.Members {
			if member == ksaMember {
				if _, present := ksaRoles[binding.Role]; present {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func getGSAAPIResource(gsaEmail string) string {
	sp := strings.Split(gsaEmail, "@")
	sp = strings.Split(sp[1], ".")
	proj := sp[0]
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", proj, gsaEmail)
}

func ksaIAMPolicyMember(wiPool, ns, ksaName string) string {
	return fmt.Sprintf("serviceAccount:%s[%s/%s]", wiPool, ns, ksaName)
}

func getClusterAPIName(project, location, name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, location, name)
}
func getWIPool(ctx context.Context, clusterAPIName string) (string, error) {
	gkeSVC, err := container.NewService(ctx)
	if err != nil {
		return "", fmt.Errorf("creating GKE.Service: %w", err)
	}

	cluster, err := gkeSVC.Projects.Locations.Clusters.Get(clusterAPIName).Do()
	if err != nil {
		return "", fmt.Errorf("getting GKE Cluster %q: %w", clusterAPIName, err)
	}
	return cluster.WorkloadIdentityConfig.WorkloadPool, nil
}

func getGSAsRolesOnProject(ctx context.Context, project, gsaEmail string) ([]string, error) {
	crmSVC, err := cloudresourcemanager.NewService(ctx)
	if err != nil {
		return []string{}, fmt.Errorf("creating CloudResourceManager.Service: %w", err)
	}
	projSVC := cloudresourcemanager.NewProjectsService(crmSVC)
	iamPolicy, err := projSVC.GetIamPolicy(project, &cloudresourcemanager.GetIamPolicyRequest{}).Do()
	if err != nil {
		return []string{}, fmt.Errorf("getting Project %q IAMPolicy: %w", project, err)
	}
	gsaMember := gsaIAMPolicyMember(gsaEmail)
	var roles []string
	for _, binding := range iamPolicy.Bindings {
		for _, member := range binding.Members {
			if member == gsaMember {
				roles = append(roles, binding.Role)
				break
			}
		}
	}
	return roles, nil
}

func gsaIAMPolicyMember(gsaEmail string) string {
	return fmt.Sprintf("serviceAccount:%s", gsaEmail)
}

func determineProject(projectFlagValue string) (string, error) {
	if projectFlagValue != "" {
		return projectFlagValue, nil
	}
	cmd := exec.Command("gcloud", "config", "get-value", "core/project")
	o, err := cmd.Output()
	if err != nil {
		return "", err
	}
	p := string(o)
	return strings.TrimSpace(p), nil
}

func GetRESTConfig(serverURL, kubeconfig string) (*rest.Config, error) {
	// If we have an explicit indication of where the kubernetes config lives, read that.
	if kubeconfig != "" {
		c, err := clientcmd.BuildConfigFromFlags(serverURL, kubeconfig)
		if err != nil {
			return nil, err
		}
		return c, nil
	}

	// If not, try the in-cluster config.
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}

	// If no in-cluster config, try the default location in the user's home directory.
	if usr, err := user.Current(); err == nil {
		if c, err := clientcmd.BuildConfigFromFlags("", filepath.Join(usr.HomeDir, ".kube", "config")); err == nil {
			return c, nil
		}
	}

	return nil, errors.New("could not create a valid kubeconfig")
}

func getClusterFromKubeconfig() (string, string, string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", "", "", nil
	}
	fp := filepath.Join(usr.HomeDir, ".kube", "config")
	f, err := os.Open(fp)
	if err != nil {
		return "", "", "", err
	}
	d := yaml.NewDecoder(f)
	type kubeconfig struct {
		CurrentContext string `yaml:"current-context"`
	}
	kc := &kubeconfig{}
	err = d.Decode(kc)
	if err != nil {
		return "", "", "", err
	}
	sp := strings.Split(kc.CurrentContext, "_")
	return sp[1], sp[2], sp[3], nil
}
