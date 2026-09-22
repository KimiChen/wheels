-- 个人自定义节点（旧站的「个人自定义上游 / SOCKS5 落地」）。
--
-- 一个人可以贴自己的分享链接建**自定义上游**，也可以建一条 **SOCKS5 落地**
-- 并指定从哪条上游拨出去。两者都只进这个人自己的订阅，别人看不到。
--
-- # 这些流量不进账本，也不受额度约束
--
-- 自定义上游的出口不是我们的节点，`user_stats` 采不到它，配额闸也够不着。
-- 页面上的用量与额度**只统计受管节点**，这条限制要在界面上说出来——
-- 不说的话，一个把大部分流量走自定义上游的人会以为自己「几乎没用量」，
-- 而那个数字是真的，只是含义和他以为的不一样。
--
-- # 为什么配置是密文
--
-- 里面是**别人家的凭据**：SS 密码、VMess/VLESS 的 UUID、SOCKS5 的用户名口令。
-- 账本要按 R3 做一致性备份并演练恢复，而那份备份会被复制到别处
-- （uPSK 不进库就是这个理由，见 `render-subscription-credentials.py`）。
-- 明文存进去，每一份账本备份都变成一份用户第三方凭据的副本。
--
-- 密钥在 `server_secrets` 之外的配置目录里，备份账本不连带备份它。
-- AEAD 的 AAD 绑 `custom-<kind>:<user_id>`，所以一条密文**换个用户、
-- 换张表都解不开**——光有写库权限不足以把 A 的上游挪给 B。

-- 自定义上游。一条分享链接解析出来的 Clash 代理配置。
CREATE TABLE custom_proxies (
    custom_proxy_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL REFERENCES users(user_id),
    -- 显示名。渲染进订阅时会加 `Custom-` 前缀，库里存的是不带前缀的原名——
    -- 前缀是**渲染口径**，改前缀不该要求改库。
    name              TEXT NOT NULL,
    -- 闭集。放开它等于放开解析器支持的协议集合，两处必须一起改。
    protocol          TEXT NOT NULL CHECK(protocol IN ('ss', 'trojan', 'vmess', 'vless')),
    -- 密文的格式版本，不是配置的版本。换 AEAD 或换编码时它加一，
    -- 解密侧按它分支——没有这一列，换格式就只能全表重写。
    config_version    INTEGER NOT NULL CHECK(config_version >= 1),
    config_ciphertext TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    -- 同一个人下重名会让订阅里出现两个同名节点，Clash 拒整份配置。
    UNIQUE(user_id, name)
) STRICT;

CREATE INDEX idx_custom_proxies_user ON custom_proxies(user_id, custom_proxy_id);

-- SOCKS5 落地。它比自定义上游多一样东西：`dialer_proxy`——从哪条上游拨出去。
--
-- **那个引用在密文里，所以不能做成外键。** 它指向的可能是 `DIRECT`、
-- 某条受管入口，或这个人自己的某条自定义上游；三者不在同一张表里，
-- 而且值本身是加密的。完整性只能在服务层与渲染时判：
--   * 建/改时校验 dialer 在「这个人当前可见的上游」里（`custom::service`）；
--   * 删/改名一条被引用的上游时拒绝（同上）；
--   * 渲染时再判一次，判不过就整份订阅失败而不是发一份坏的。
-- 三道里最后那道是兜底：前两道都在应用层，而应用层会被绕过。
CREATE TABLE custom_socks5 (
    custom_socks5_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL REFERENCES users(user_id),
    name              TEXT NOT NULL,
    config_version    INTEGER NOT NULL CHECK(config_version >= 1),
    config_ciphertext TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE(user_id, name)
) STRICT;

CREATE INDEX idx_custom_socks5_user ON custom_socks5(user_id, custom_socks5_id);
