-- 024: 用户配额、账号锁定与档案字段（设计 3.2.1/6.2.1/6.3.1/6.12.4/6.1.3/7.3，C3/C6/C9/C21a）。
-- storage_quota：个人空间存储配额（字节），默认 10GiB；新用户开户默认经
--   system_settings 的 upload.default_quota 热读取（UserStore.CreateUser 回填）。
-- failed_login_count / locked_until：连续登录失败锁定（C9）——达到
--   LOGIN_MAX_RETRIES（默认 5）次后 locked_until = now() + LOGIN_LOCK_MINUTES
--   （默认 15 分钟）并清零计数；成功登录清零。锁定判定按 locked_until 时间
--   比较（users.status 不改写，仍为 active），到期自动解锁。
-- nickname/department/position/phone/bio/language/timezone：用户档案（C21a）；
--   头像不落库（avatar_text 由前端按用户名/昵称首字母计算）。
-- 注意：软删除（回收站）文件计入已用配额——底层对象在彻底清理前仍占用存储
--（files.UsedStorage 不排除 deleted_at，与仪表盘统计口径不同）。
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS storage_quota BIGINT NOT NULL DEFAULT 10737418240,
    ADD COLUMN IF NOT EXISTS storage_used BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS failed_login_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS locked_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS nickname VARCHAR(64),
    ADD COLUMN IF NOT EXISTS department VARCHAR(128),
    ADD COLUMN IF NOT EXISTS position VARCHAR(128),
    ADD COLUMN IF NOT EXISTS phone VARCHAR(32),
    ADD COLUMN IF NOT EXISTS bio VARCHAR(512),
    ADD COLUMN IF NOT EXISTS language VARCHAR(8) NOT NULL DEFAULT 'zh-CN',
    ADD COLUMN IF NOT EXISTS timezone VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai';
