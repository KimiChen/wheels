## 项目工作要求
 - 项目所有文件默认 UTF-8 格式
 - 根目录的每个文件夹是一个子项目，各个子项目默认配置放置在其根目录的.env文件中
 - 会话开始前先确定子项目的英文id，并输出到会话中
 - 子项目文档放置在其根目录，文件名为README.md
 - vps 信息在 ~/work/daily/docs/vps.md 文件中

 ## Git 约定
 
 - 默认 git 是个公开 git
 - 用户已要求：以后每次改动后，按改动范围自己 先 `git pull`、`git add` 和 `git commit`，然后 `git push`，需要的 PrivateKey 路径 和 Passphrase 在 .env 文件中
 - 提交只包含本轮相关文件，不要把无关生成物混进去。
 - 生成物和依赖目录应保持 ignored，写入.gitignore
 - 提交信息明确，例如：
   - `修复：调整窗口标题栏布局`
   - `文档：添加代理指南`
   - `新增：更新工作区路径`
 - git pull、git push 的 PrivateKey 和 Passphrase 在 .env 文件中
 - Windows PowerShell 下已验证可用的 git pull/push 方式：
   - 从 `.env` 读取 `PrivateKey` 和 `Passphrase`，不要把值输出到终端。
   - 使用临时 `SSH_ASKPASS` 脚本把 passphrase 传给 `ssh`，不要依赖本机 `ssh-agent` 服务。
   - `GIT_SSH_COMMAND` 使用 `ssh -i "$keyPath" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new`，避免写未转义的 Windows 绝对路径可执行文件名。
   - 每次执行 `git pull` / `git push` 后检查 `$LASTEXITCODE`，PowerShell 不会自动把所有外部命令失败变成终止错误。
   
 ## Windows PowerShell 约定

 - 在 Windows PowerShell 下读取本仓库中文 Markdown 文件时，必须显式指定 UTF-8，避免控制台按系统默认编码读取后出现乱码：
   - `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Get-Content -Raw -Encoding UTF8 .\AGENTS.md`
   - 逐行读取时使用：`Get-Content -Encoding UTF8 .\AGENTS.md`
 - 写入中文文本时也应显式指定 UTF-8，例如：`Set-Content -Encoding UTF8`。
