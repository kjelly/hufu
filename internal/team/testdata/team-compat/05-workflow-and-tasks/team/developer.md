---
name: developer
description: Implements the change during the execute phase
role: worker
tools: view,write,edit,multiedit,grep,glob,ls,bash
side_effect: workspace_write
recovery: retry
---
Implement the requested change during the execute phase.
