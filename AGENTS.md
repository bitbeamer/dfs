# DFS agent guidance

## Version control

- This is a colocated Jujutsu/Git repository (`.jj/` + `.git/`). Use `jj` for
  commits and history changes; do not use `git commit`, `git rebase`, or other
  history-mutating Git commands.
- Standing authorization: after a task or goal is completed and verified,
  commit the working-copy change with Jujutsu without asking for confirmation
  each time — `jj describe -m "<summary>"` followed by `jj new` (equivalently
  `jj commit -m "<summary>"`).
- Commit messages are short imperative subject lines (for example "Bound
  interactive content reads"); match the style of `jj log`. Keep one logical
  change per commit.
