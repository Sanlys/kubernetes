{{/*
  Make containerd chown device nodes passed into a container (raw block
  volumes, e.g. volumeMode: Block PVCs, and other devices) to the pod's
  runAsUser/runAsGroup instead of leaving them root:root. Without this,
  non-root containers can't open the block devices they're given.

  Talos >= 1.14 has a dedicated CRICustomizationConfig document for CRI
  fragments; the old machine.files /etc/cri/conf.d/20-customization.part
  approach is deprecated (and its name "customization" is reserved, so a
  CRICustomizationConfig can't reuse it). Branch on the node's *running*
  version, like node/<host>/00-installation.yaml.tpl, so this renders a
  valid config for nodes still on 1.13.x as well.
*/}}
{{ if semverCompare ">= 1.14.0-0" .Node.RuntimeData.TalosVersion -}}
apiVersion: v1alpha1
kind: CRICustomizationConfig
name: device-ownership
content: |
  [plugins."io.containerd.cri.v1.runtime"]
    device_ownership_from_security_context = true
{{ else -}}
machine:
  files:
    - op: create
      path: /etc/cri/conf.d/20-customization.part
      permissions: 0o644
      content: |
        [plugins."io.containerd.cri.v1.runtime"]
          device_ownership_from_security_context = true
{{ end -}}
