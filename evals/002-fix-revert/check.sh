#!/bin/sh
# 在 main 的干净副本上加隐藏测试再跑。
cp "$EVAL_CASE/_hidden/total_hidden_test.go" . || exit 2
exec go test ./...
