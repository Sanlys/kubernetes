{{/*
  Talos >= 1.14 selects the install disk via the UnattendedInstallConfig
  document; older versions use the now-deprecated machine.install field.
  Branch on the node's *running* version (populated by `topf apply`/
  `topf render --online`) so this keeps working mid-rollout while some
  nodes are still on 1.13.x and others have already moved to 1.14.1.
*/}}
{{ if semverCompare ">= 1.14.0-0" .Node.RuntimeData.TalosVersion -}}
apiVersion: v1alpha1
kind: UnattendedInstallConfig
provisioning:
  diskSelector:
    match: disk.wwid == "naa.50026b723700d6dd"
  wipe: false
{{ else -}}
machine:
  install:
    diskSelector:
      wwid: naa.50026b723700d6dd
{{ end -}}
