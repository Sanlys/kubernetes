# To connect via rcon:
kubectl exec -n minecraft-server -it deploy/mc-paper -c minecraft -- rcon-cli --password "supersecret" --port 25575