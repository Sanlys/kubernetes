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
  shellHook = ''
    ./decrypt_talos.sh
    export TALOSCONFIG=$(realpath "./talos_dec/talosconfig")
    export EDITOR=nvim
    tmux
  '';

}

