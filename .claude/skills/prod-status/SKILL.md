---
name: prod-status
description: One read-only pass over production (app.gocov.dev on ECS Fargate) — service rollout, running image vs latest release, task health, alarms, ALB targets, /healthz, recent deploy runs — and a verdict.
allowed-tools: Bash, Read, Grep, Glob
---

# Production status

app.gocov.dev is one Fargate task: cluster `gocov`, service `gocov-server`, region
`eu-central-1`, behind ALB target group `gocov-server`, logs in `/gocov/server`, four
CloudWatch alarms on SNS `gocov-alarms`. `deploy/README.md` is the runbook. Everything
below is read-only; this skill never changes the service.

Run these, in this order, with `AWS_REGION=eu-central-1`:

```sh
aws sts get-caller-identity --query Account --output text
```
If this fails the local AWS session has expired: tell the user to run `aws login` and
stop. Expected account: 773658094601.

```sh
aws ecs describe-services --cluster gocov --services gocov-server \
  --query 'services[0].{status:status,desired:desiredCount,running:runningCount,deployments:deployments[].{status:status,rollout:rolloutState,taskDef:taskDefinition,running:runningCount,updated:updatedAt}}'
t=$(aws ecs list-tasks --cluster gocov --service-name gocov-server --query taskArns --output text)
aws ecs describe-tasks --cluster gocov --tasks $t \
  --query 'tasks[].{image:containers[0].image,health:healthStatus,last:lastStatus,started:startedAt}' --output table
gh release view -R gocov/gocov --json tagName,publishedAt -q '"\(.tagName) \(.publishedAt)"'
aws cloudwatch describe-alarms --query 'MetricAlarms[].{name:AlarmName,state:StateValue,since:StateUpdatedTimestamp}' --output table
tg=$(aws elbv2 describe-target-groups --names gocov-server --query 'TargetGroups[0].TargetGroupArn' --output text)
aws elbv2 describe-target-health --target-group-arn "$tg" \
  --query 'TargetHealthDescriptions[].{target:Target.Id,state:TargetHealth.State,reason:TargetHealth.Reason}' --output table
curl https://app.gocov.dev/healthz -sS -m 10 -o /dev/null -w '%{http_code} in %{time_total}s\n'
gh run list --workflow release-please.yml -L 3 --json conclusion,createdAt,displayTitle,url
gh run list --workflow deploy.yml -L 3 --json conclusion,createdAt,displayTitle,url
aws logs tail /gocov/server --since 30m --format short | grep -iE 'error|panic|level=error' | tail -20
```

## How to read it

- **Healthy** means: one PRIMARY deployment with `rolloutState: COMPLETED`, running
  equals desired, the task `HEALTHY` and `RUNNING`, its image tag equal to the latest
  release tag, the ALB target `healthy`, `/healthz` 200, every alarm `OK`
  (`gocov-unhealthy-targets` sits in `INSUFFICIENT_DATA` when nothing has been
  unhealthy; that is normal).
- **Rollout still IN_PROGRESS** with two deployments: a deploy is mid-flight. Say which
  revision is PRIMARY and which is draining. Do not call it failed.
- **A red release/deploy run but ECS shows PRIMARY COMPLETED on the new tag**: the deploy
  succeeded; `deploy.yml` samples `rolloutState` once right after the stability waiter
  and can lose that race. Say so explicitly, and note that the smoke steps after the
  check were skipped, so a real upload has not been proven for that release.
- **Image tag behind the latest release**: the release was cut but not deployed (the
  production approval was not clicked, or the deploy job failed before rolling).
  Point at the run URL. The rollback and the manual deploy are the same command,
  which the user runs, never you: `gh workflow run deploy.yml -f tag=vX.Y.Z`.
- **Image from ECR `gocov-server-dev`**: a branch deploy from a laptop
  (`scripts/deploy-branch.sh`) is live, not a release. Say which tag, and that the
  release image is the rollback.
- **Alarm in ALARM**: name it, quote `since`, and pull the matching signal (5xx: recent
  log errors and target health; cpu/memory: `describe-tasks` plus the last 30 minutes of
  logs) before suggesting anything.

Report as a short verdict line first ("Production is on v0.21.0, healthy, no alarms"),
then only the rows that are not nominal. Never run `update-service`, `deploy-branch.sh`,
`gh workflow run`, or anything that writes.
