# Tekton is manually installed, as there is no official helm chart or kustomize

## Latest official stable release for Tekton Pipelines

```bash
kubectl apply --filename https://storage.googleapis.com/tekton-releases/pipeline/latest/release.yaml
```

Monitor installation
```bash
kubectl get pods --namespace tekton-pipelines --watch
```

## Latest official stable release for Tekton Triggers

```bash
kubectl apply --filename https://storage.googleapis.com/tekton-releases/triggers/latest/release.yaml
kubectl apply --filename https://storage.googleapis.com/tekton-releases/triggers/latest/interceptors.yaml
```

Monitor installation
```bash
kubectl get pods --namespace tekton-pipelines --watch
```

## Latest official stable release for Tekton Dashboard

```bash
kubectl apply --filename https://infra.tekton.dev/tekton-releases/dashboard/latest/release-full.yaml
```

Manually edit the ingress it made, to allow cert-manager to fetch the cert
```bash
kubectl -n tekton-pipelines edit ingress/tekton-dashboard
```

```kubectl
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"annotations":{},"name":"tekton-dashboard","namespace":"tekton-pipelines"},"spec":{"rules":[{"host":"tekton.k8s.lysakermoen.com","http":{"paths":[{"backend":{"service":{"name":"tekton-dashboard","port":{"number":9097}}},"pathType":"ImplementationSpecific"}]}}],"tls":[{"hosts":["tekton.k8s.lysakermoen.com"],"secretName":"tekton-dashboard-cert"}]}}
    cert-manager.io/cluster-issuer: letsencrypt-internal
  creationTimestamp: "2025-12-03T20:14:05Z"
  generation: 1
  name: tekton-dashboard
  namespace: tekton-pipelines
  resourceVersion: "33994929"
  uid: ee54ce9f-990b-45fa-99af-ade92b78a95c
spec:
  ingressClassName: internal
  rules:
  - host: tekton.k8s.lysakermoen.com
    http:
      paths:
      - backend:
          service:
            name: tekton-dashboard
            port:
              number: 9097
        pathType: ImplementationSpecific
  tls:
  - hosts:
    - tekton.k8s.lysakermoen.com
    secretName: tekton-dashboard-cert
status:
  loadBalancer:
    ingress:
    - ip: 10.0.6.240
```