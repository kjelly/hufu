#!/usr/bin/env bash

cd /home/kjelly/github/pilot
# hufu --default --route team --helper-tools bash,terminal "依照現有的docs/runbooks/minimal-poc-architecture.md從零重新驗證一次runbook. 你可以重建vm，自行產生相關vault, 使用互動式" --model ollama/minimax-m3:cloud --timeout 7600 --new

hufu --default --route team --helper-tools bash,terminal "continue" --model ollama/minimax-m3:cloud --timeout 7600 --max-rounds 50 --max-steps 100

這是/home/kjelly/github/pilot/workspace hufu執行任務的 workspace, hufu 目前有什麼可以改善的地方嗎?
