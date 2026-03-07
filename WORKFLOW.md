---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: demo
codex:
  command: codex app-server
---

你正在处理一个来自 Linear 的 issue。

- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

请先理解问题，再按仓库工作流完成开发任务。
