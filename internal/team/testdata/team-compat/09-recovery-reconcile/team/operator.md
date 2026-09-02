---
name: operator
description: Performs infrastructure changes recovered by reconciliation, not blind retry
role: worker
tools: view,grep,glob,ls,bash
side_effect: infra_mutation
recovery: reconcile
reconcile-tool: "true"
---
Make the requested infrastructure change. If interrupted, the runtime reconciles state instead of blindly retrying.
