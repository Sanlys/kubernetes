# Without this, Talos's auto-generated hostname is only stable as long as
# nothing fully re-applies machine config from scratch. The first topf
# apply against a node (a fresh, differently-structured config bundle
# compared to whatever talhelper had last pushed) rerolled at least one
# node's auto-generated hostname on the reboot it required, which made
# kubelet re-register as a brand new Kubernetes Node object under the new
# name, orphaning the old one. Pin the hostname explicitly to what
# topf.yaml already calls the node, matching its pre-migration name, so
# this can't happen again on any other node during the rollout.
apiVersion: v1alpha1
kind: HostnameConfig
auto: "off"
hostname: {{ .Node.Host }}
