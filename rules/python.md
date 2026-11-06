---
paths:
  - "**/*.py"
---
# Python
- 用项目已有的环境和工具（uv、poetry、venv），不往系统 Python 里装包。新依赖写进项目的依赖文件。
- 新函数写类型标注。不写裸 `except:`，捕获具体异常，要么处理要么带上下文抛出。
- `requests`、`httpx`、`subprocess.run` 都设 `timeout`；子进程用参数列表和 `check=True`，不拼字符串走 shell。
- 文件用 `with` 打开并写明 `encoding`，路径用 `pathlib`。
- 交付前：跑项目配置的 lint 和类型检查（常见是 `ruff`、`mypy`），再跑 `pytest`。
