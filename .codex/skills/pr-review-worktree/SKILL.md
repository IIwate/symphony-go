---
name: pr-review-worktree
description:
  使用 `.codex-review/pr<PR号>` git worktree 隔离审查远程 GitHub PR；在
  worktree 中直接修改并推回 PR 分支，验证通过后合并，并清理本地审查
  worktree。适用于需要审查、修复并落地现有 PR 的场景。
---

# PR Review Worktree

## Goals

- 为每个 PR 建立独立的 `.codex-review/pr<PR号>` worktree。
- 在隔离目录中审查和修改代码，不污染主工作区。
- 若 PR 有问题，直接把修复提交并推回该 PR 的远程分支。
- 若 PR 没问题或修复完成，完成合并并清理本地审查 worktree。

## Rules

- `.codex-review/` 是本地临时审查区，不提交到仓库。
- 审查 worktree 命名固定为 `.codex-review/pr<PR号>`。
- 本地审查分支命名固定为 `review/pr<PR号>`。
- 默认优先复用 `origin`；若 PR 来自 fork，再临时添加 `pr-<PR号>` remote。
- 若 fork PR 的 `maintainer_can_modify=false`，不要直接修改，先报告阻塞。

## Inputs

- PR 编号，例如 `48`
- 当前仓库已通过 `gh auth status`

## Inspect PR

先读取 PR 元数据，确认 head branch、head repo 和是否允许维护者修改：

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
pr=48

gh api "repos/$repo/pulls/$pr" --jq '{
  number: .number,
  title: .title,
  base: .base.ref,
  head_ref: .head.ref,
  head_repo: .head.repo.full_name,
  head_clone: .head.repo.clone_url,
  same_repo: (.head.repo.full_name == .base.repo.full_name),
  maintainer_can_modify: .maintainer_can_modify
}'
```

## Create Worktree

### Same-repo PR

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
pr=48
head_ref=$(gh api "repos/$repo/pulls/$pr" --jq .head.ref)

git fetch origin "$head_ref"
git worktree add -B "review/pr$pr" ".codex-review/pr$pr" "origin/$head_ref"
```

### Fork PR

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
pr=48
head_ref=$(gh api "repos/$repo/pulls/$pr" --jq .head.ref)
head_clone=$(gh api "repos/$repo/pulls/$pr" --jq .head.repo.clone_url)
can_modify=$(gh api "repos/$repo/pulls/$pr" --jq .maintainer_can_modify)

test "$can_modify" = "true"

remote="pr-$pr"
git remote add "$remote" "$head_clone" 2>/dev/null || git remote set-url "$remote" "$head_clone"
git fetch "$remote" "$head_ref"
git worktree add -B "review/pr$pr" ".codex-review/pr$pr" "$remote/$head_ref"
```

## Review Workflow

1. 在主工作区保持 `main` 干净，不在主工作区直接改 PR 代码。
2. 进入 `.codex-review/pr<PR号>` 做审查、测试和修改。
3. 若只做审查不改代码，直接给出 findings 或进入合并流程。
4. 若需要修复，提交必须发生在 `.codex-review/pr<PR号>` 里。
5. 推送时显式把当前 HEAD 推回 PR 的远程 head branch，不依赖本地分支同名。

## Push Fixes

### Same-repo PR

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
pr=48
head_ref=$(gh api "repos/$repo/pulls/$pr" --jq .head.ref)

git -C ".codex-review/pr$pr" status --short
git -C ".codex-review/pr$pr" add -A
git -C ".codex-review/pr$pr" commit
git -C ".codex-review/pr$pr" push origin HEAD:"$head_ref"
```

### Fork PR

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
pr=48
head_ref=$(gh api "repos/$repo/pulls/$pr" --jq .head.ref)
remote="pr-$pr"

git -C ".codex-review/pr$pr" status --short
git -C ".codex-review/pr$pr" add -A
git -C ".codex-review/pr$pr" commit
git -C ".codex-review/pr$pr" push "$remote" HEAD:"$head_ref"
```

## Merge

确认 review 和 checks 都满足要求后，再合并：

```bash
pr=48
gh pr view "$pr" --json mergeable,state,url
gh pr checks "$pr"
gh pr merge "$pr" --squash
```

若需要持续盯 checks 或处理 review comments，可配合现有 `land` 技能。

## Cleanup

审查结束后始终清理本地 worktree：

```bash
pr=48
git worktree remove ".codex-review/pr$pr"
git branch -D "review/pr$pr"
```

若使用过 fork 临时 remote，再删掉：

```bash
pr=48
git remote remove "pr-$pr"
```

## Safety Checks

- 若 `.codex-review/pr<PR号>` 已存在，先检查是否有未提交修改，再决定复用或删除。
- 不要把 `.codex-review/` 路径下的文件加入 commit。
- 不要在主工作区切到 PR head branch；主工作区保留给 `main` 和正常开发。
- 推送前在 worktree 内执行对应验证命令，例如 `go test ./...`。
