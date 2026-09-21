# Security

Do not include credentials in issues, logs or pull requests. Use GitHub private vulnerability reporting for security defects. Runtime/operator tokens, kubeconfig, signing keys and local state must not be committed.

Production requires HTTPS control traffic, Kubernetes CA verification, scoped service-account credentials, an immutable game image digest and a private admin endpoint. The local harness is loopback-only development infrastructure with a deliberately simplified protocol fixture. It must not be exposed publicly or used as a game server.
