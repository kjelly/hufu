#!/usr/bin/env bash
cd /home/ubuntu/nfs/github/tmux-ai
go run . pingpong \
  --preset ./presets/review-fix.yaml \
  --pane1 hufu:3 \
  --pane2 hufu:1 \
  --start pane1
