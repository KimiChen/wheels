-- 订阅 token（README §4.7）。
--
-- **只存哈希，明文只在创建时显示一次。** 库被读到也不能直接拉走别人的订阅——
-- 与会话 ID 同一条纪律。
--
-- **没有自助轮换。** 轮换会让该用户所有已导入的客户端**静默失效**，
-- 而用户通常不知道要重新导入。吊销是运维动作，走命令行、需二次确认、落审计。
-- 所以这张表没有 rotated_at，也没有「历史 token」的概念：
-- 一个用户一条有效 token，吊销之后要重新发就是重新创建一条。
CREATE TABLE subscription_tokens (
    token_pk       INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 查表用的摘要。**明文不存**。
    token_sha256   TEXT NOT NULL UNIQUE CHECK(length(token_sha256) = 64),
    user_id        INTEGER NOT NULL REFERENCES users(user_id),
    -- 第几代。token 是 `HMAC(服务端密钥, user_id || generation)` **派生**出来的，
    -- 不是随机生成后存起来的。
    --
    -- 这样解决了 §4.7 自己的一处矛盾：那一节既要求「只存哈希」，
    -- 又要求 token「之后只读」。两者不能同时成立——除非它是可重算的。
    -- 派生之后：明文一个字节都不落库，而本人随时能再看一次自己的订阅地址；
    -- 吊销就是 generation 加一，旧 token 立刻算不出来也查不到。
    generation     INTEGER NOT NULL CHECK(generation >= 1),
    created_at     TEXT NOT NULL,
    -- 吊销后立刻失效。**每次取订阅都查**，与会话吊销同一条纪律。
    revoked_at     TEXT,
    -- 最后一次被拉取的时刻。用来回答「这个人还在用吗」——
    -- 吊销一个从没被用过的 token 和吊销一个每天在用的 token，风险完全不同。
    last_used_at   TEXT
) STRICT;

-- 一个用户同一时刻只能有一条未吊销的 token。
-- 没有它，一次重复的建号会悄悄发出第二条，而第一条仍然有效——
-- 于是吊销一条不等于断掉这个人。
CREATE UNIQUE INDEX idx_subscription_active_per_user
    ON subscription_tokens(user_id) WHERE revoked_at IS NULL;
