Use this tool to help diagnose problems with GKE workload identity.

## Build

```
go build -o diagnose-wi cmd/diagnose-wi/main.go
```

## Usage

### Prerequisites
1. Make sure `kubectl` is pointed at the correct GKE cluster.
1. Make sure `gcloud` is setup and has authentication sufficient to get IAM policies.

### Examples

Check the `agent` KSA in the `my-ns` namespace.

```
diagnose-wi -ns my-ns -ksa agent
```

Check the KSA being used by Pod `my-pod` in the `my-ns` namespace.

```
diagnose-wi -ns my-ns -pod my-pod
```

Check the `agent` KSA in the `my-ns` namespace with permissions on the GCP project `other-project`.

```
diagnose-wi -ns my-ns -ksa agent -project other-project
```

## Common permission issues

### KSA does not have permission on the GSA

> Pod "agent-8948bd7b-vz5wp" uses KSA "agent", which links to GSA "1234567890123-compute@developer.gserviceaccount.com", but that GSA does not grant access to the KSA

```
gcloud iam service-accounts add-iam-policy-binding \
  --project "${PROJECT}" \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:${PROJECT}.svc.id.goog[${NAMESPACE}/${KSA}]" \
  "${GSA}"
```

