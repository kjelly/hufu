---
name: verifier
description: Confirms the review report exists
role: worker
tools: view,ls
side_effect: none
recovery: retry
---
Confirm that the review report was written.
