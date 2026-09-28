## 项目工作要求
 - 项目所有文件默认 UTF-8 格式
 - 根目录的每个文件夹是一个子项目，各个子项目默认配置放置在其根目录的.env文件中
 - 会话开始前先确定子项目的英文id，并输出到会话中
 - 子项目文档放置在其根目录，文件名为README.md
 - 服务器信息在 ../daily/docs/vps.md 文件中

 ## Git 约定
 
 - 默认 git 是个公开 git，注意提交的内容需要脱敏（包括单不限于域名、IP、证书、密码、密钥、配置）
 - 用户已要求：以后每次改动后，按改动范围自己 先 `git pull --rebase`、`git add` 和 `git commit`，需要的 PrivateKey 路径 和 Passphrase 在 .env 文件中
 - 提交只包含本轮相关文件，不要把无关生成物混进去。
 - 生成物和依赖目录应保持 ignored，写入.gitignore
 - 提交信息明确，例如：
   - `修复：调整窗口标题栏布局`
   - `文档：添加代理指南`
   - `新增：更新工作区路径`
 - git pull、git push 的 PrivateKey 和 Passphrase 在 .env 文件中
