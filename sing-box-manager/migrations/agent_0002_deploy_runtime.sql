-- Agent 本地库 0002：部署所需的磁盘配置指针与 epoch。
-- 保留当前与上一个成功 revision 的磁盘配置路径，供本机原子回滚（无需 Manager 重推明文）。

ALTER TABLE local_revisions ADD COLUMN config_path   TEXT;
ALTER TABLE local_revisions ADD COLUMN role          TEXT;
ALTER TABLE local_revisions ADD COLUMN runtime_epoch INTEGER;
ALTER TABLE local_revisions ADD COLUMN succeeded     INTEGER NOT NULL DEFAULT 1;
