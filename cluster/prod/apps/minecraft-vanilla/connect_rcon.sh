# To connect via rcon:
kubectl exec -n minecraft-vanilla -it deploy/mc-vanilla -c minecraft -- rcon-cli --password "supersecret" --port 25575
