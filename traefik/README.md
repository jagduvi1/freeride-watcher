# Shared Traefik reverse proxy

This directory contains the Docker Compose configuration for a shared Traefik reverse proxy that lets you run **multiple projects** (e.g. Cellarion + freeride-watcher) on the same VM using Cloudflare as the SSL layer.

## Architecture

```
Browser
  │ HTTPS (Cloudflare edge certificate)
  ▼
Cloudflare (proxy, always-use-HTTPS, Full SSL mode)
  │ HTTP on port 80
  ▼
VM: Traefik container
  ├── cellarion.app      → cellarion-frontend container (port 80)
  └── freerider.app      → freeride-watcher container  (port 8080)
```

Traefik routes by hostname. Each app only needs to be on the shared `web` Docker network with the right labels — no port bindings to the host.

---

## One-time VM setup

```bash
# 1. Create the shared Docker network (once per VM, ever).
docker network create web

# 2. Start Traefik.
cd traefik
docker compose up -d
```

---

## Cloudflare settings (per domain)

In Cloudflare Dashboard for **each domain** you route through this VM:

| Setting | Value |
|---|---|
| DNS → A record | VM public IP |
| DNS → Proxy status | **Proxied** (orange cloud ON) |
| SSL/TLS → Mode | **Full** |
| SSL/TLS → Edge Certificates → Always Use HTTPS | **ON** |

> **Why "Full" and not "Flexible"?**
> Flexible means Cloudflare sends plain HTTP to your server with no validation.
> Full checks that your server responds on HTTPS — but in this setup Traefik only
> listens on port 80 (HTTP), which is fine because Cloudflare's edge handles
> the browser-facing HTTPS. Use Full so at least CF verifies the connection succeeds.
>
> If you ever want end-to-end encryption (CF → Traefik over HTTPS), install a
> Cloudflare Origin Certificate in Traefik and switch to Full (Strict). See below.

---

## Adding freeride-watcher

```bash
cd /path/to/freeride-watcher
# Set DOMAIN=freerider.yourdomain.com in .env
docker compose -f docker-compose.prod.yml up -d --build
```

Traefik picks up the container immediately via Docker labels.

---

## Adding Cellarion (changes to Cellarion's docker-compose.yml)

Remove the hard-coded port binding from Cellarion's frontend service and add Traefik labels:

```yaml
# Cellarion docker-compose.yml — diff

services:
  frontend:
    build: ./frontend
-   ports:
-     - "80:80"
+   expose:
+     - "80"
+   networks:
+     - web
+     - default
+   labels:
+     traefik.enable: "true"
+     traefik.docker.network: "web"
+     traefik.http.routers.cellarion.rule: "Host(`cellarion.app`)"
+     traefik.http.routers.cellarion.entrypoints: "web"
+     traefik.http.services.cellarion.loadbalancer.server.port: "80"
    depends_on:
      backend:
        condition: service_healthy

+networks:
+  web:
+    external: true
```

Then restart Cellarion: `docker compose up -d`

---

## Optional: end-to-end TLS with Cloudflare Origin Certificate

If you want to encrypt the Cloudflare → VM leg too (recommended for production):

1. Cloudflare → SSL/TLS → Origin Server → **Create Certificate**
   - Hostnames: `*.yourdomain.com`, `yourdomain.com`
   - Validity: 15 years
   - Key type: RSA
2. Save the two files to `traefik/certs/origin.crt` and `traefik/certs/origin.key`
3. Add a volume mount and a dynamic TLS config to `traefik/docker-compose.yml`:

```yaml
# In the traefik service:
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
  - letsencrypt:/letsencrypt
  - ./certs:/certs:ro           # ← add this
  - ./dynamic.yml:/dynamic.yml:ro  # ← add this

command:
  # ... existing flags ...
  - --entrypoints.web.address=:80
  - --entrypoints.websecure.address=:443   # ← add 443
  - --providers.file.filename=/dynamic.yml # ← add file provider
```

```yaml
# traefik/dynamic.yml
tls:
  certificates:
    - certFile: /certs/origin.crt
      keyFile: /certs/origin.key
  stores:
    default:
      defaultCertificate:
        certFile: /certs/origin.crt
        keyFile: /certs/origin.key
```

Then set Cloudflare SSL mode to **Full (Strict)** and update your router labels to use `websecure` instead of `web`.
