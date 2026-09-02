---
name: reviewer
description: Reviews the change during the verify phase
role: worker
tools: view,grep,glob,ls,bash
side_effect: none
recovery: retry
---
Review the implementation during the verify phase and report findings.
