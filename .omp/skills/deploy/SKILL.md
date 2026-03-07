---
name: deploy
description: Build and deploy goduckbot. Use when building Docker images, bumping versions, or deploying to k8s.
argument-hint: [version-tag]
---

# Goduckbot Deploy

Full build-tag-deploy-verify cycle for goduckbot.

> **CI/CD handles Docker image builds AND helm chart publishing on merge to master. NEVER build Docker images locally.**
> Production namespace is `cardano`, NOT `duckbot`.

## 1. Pre-flight checks

```bash
cd /home/wayne/git/goduckbot
go vet ./...
go build ./...
go test ./... -v
```

Check currently deployed version:

```bash
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].spec.containers[0].image}'
```

## 2. Version tagging

Tag format: `v{major}.{minor}.{patch}` (e.g., `v3.0.17`).

```bash
git tag v{VERSION}
git push origin v{VERSION}
```

CI/CD triggers on merge to master and builds multi-arch images (`linux/amd64`, `linux/arm64`). The helm chart is also published automatically.

## 3. Deploy

Update the image tag in the infra repo values file:

- **Production**: `/home/wayne/git/infra/helmfile-app/duckbot/values.goduckbot.yaml.gotmpl`
- **Test** (optional): `/home/wayne/git/infra/helmfile-app/duckbot/values.goduckbot-test.yaml.gotmpl`

Always diff before apply:

```bash
cd /home/wayne/git/infra/helmfile-app && helmfile -e apps -l app=goduckbot diff
```

Apply only after confirming the diff looks correct:

```bash
helmfile -e apps -l app=goduckbot apply
```

## 4. Verify

Check pods are running:

```bash
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o wide
```

Check logs for healthy operation (blocks being processed, slot/epoch visible):

```bash
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=20
```

For test deployments, use the test label:

```bash
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot-test -o wide
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot-test --tail=20
```

Verify chain sync is progressing — logs should show slots and epochs advancing.

## 5. Rollback

Revert the image tag in the infra values file to the previous version, then apply:

```bash
cd /home/wayne/git/infra/helmfile-app && helmfile -e apps -l app=goduckbot apply
```

## Notes

- Test deployments use `goduckbot-test` label and a separate values file.
- Always `diff` before `apply`.
- CI/CD handles everything after merge to master — do not build or push Docker images manually.
