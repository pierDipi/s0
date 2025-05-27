
kubectl label node kind-control-plane node.kubernetes.io/exclude-from-external-load-balancers-

export INSTALL_ISTIO=${INSTALL_ISTIO:-"false"}

# TODO: Istio installation is not tested yet
if [[ ${INSTALL_ISTIO} == "true" ]]; then
  kubectl kustomize "github.com/kubernetes-sigs/gateway-api/config/crd?ref=v1.2.1" | kubectl apply -f -

  ./istio/bin/istioctl x precheck

  ./istio/bin/istioctl install \
    -y \
    --set profile=default \
    --set components.cni.enabled=false \
    --set meshConfig.outboundTrafficPolicy.mode=REGISTRY_ONLY

  ko resolve -Rf config/manifests/istio
fi

kustomize build ./config/kustomize/ |
  ko resolve -Rf config/manifests/ -f - |
  kubectl apply --server-side --force-conflicts -f -
