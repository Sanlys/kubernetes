{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    kubectl
    talosctl
    cilium-cli
    sops
    age
    kubernetes-helm
    tmux
  ];
}

