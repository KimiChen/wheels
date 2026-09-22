-- 个人自定义分流规则。
--
-- 一个人可以加一批「这个域名 / 这个网段走我自己那一档」的规则，它们渲染成
-- 一个 `Custom-<登录名>` 代理组加一批排在**最前面**的 Clash 规则。
--
-- # 为什么是「一人一行」而不是「一条规则一行」
--
-- 整份规则表存成一个加密 blob。一条一行的话，每条的密文都带独立随机 nonce，
-- 于是**同一个域名加两次会产生两条不同的密文**——库里没法用 UNIQUE 去重，
-- 顺序也要靠额外一列维护。而规则是有序的（首条匹配即生效），
-- 顺序本身就是数据，存成一个列表最直白。
--
-- 代价：改一条要读-改-写整份。SQLite 是单写者，一个事务里做完即可。
--
-- # 为什么加密
--
-- 这是**「用户 → 域名」的明细**，与审计树同一性质。C24 特意把审计树放在账本
-- 之外，理由就是 `ledger backup` 的产物会被复制到别处，而那份明细不该跟着走。
-- 规则表比审计更直接：它写明了这个人**特意关心**哪些站点。
--
-- 密钥与加密方式跟自定义节点共用（`custom::crypto`），AAD 是
-- `custom-rules:<user_id>`——与另外两类的 AAD 都不同，所以三张表的密文
-- 互相搬不动。
CREATE TABLE custom_rules (
    -- **一人一行**，所以主键就是 user_id 本身，不另设自增列。
    user_id           INTEGER PRIMARY KEY REFERENCES users(user_id),
    config_version    INTEGER NOT NULL CHECK(config_version >= 1),
    -- 一个 JSON 数组的密文：[{kind, value}, …]，顺序即优先级。
    config_ciphertext TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;
