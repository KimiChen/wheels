# 测试

`scripts/verify-overlay.sh` 负责结构和 Git tree 一致性检查；
`scripts/test-source.sh` 在临时组装的完整源码上执行上游与 Overlay 的测试和构建。

新增 Overlay 专项测试时，优先把测试源码放在 `overlay/` 对应 package 中，使其与
实际代码一起参与上游测试体系。这里只保存跨 package、容器或部署层的专项资源。
