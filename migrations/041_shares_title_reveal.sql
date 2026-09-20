-- 041: 分享再次查看（链接/密码）与打包分享自定义标题（v2.4 整改）。
-- token / password_plain：公开分享的明文链接令牌与访问密码留存，供「我的
-- 分享」随时再次查看（产品决策：以库内明文换取可再次展示；历史行为为仅
-- 创建响应返回一次）。旧行为创建的行两列为空串，前端提示撤销后重建。
-- password_plain 仅在创建时设置了访问密码的公开分享上有值；哈希列
-- （token_hash/password_hash）仍是访问校验的唯一依据。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS token TEXT NOT NULL DEFAULT '';
ALTER TABLE shares ADD COLUMN IF NOT EXISTS password_plain TEXT NOT NULL DEFAULT '';
-- title：打包分享（is_bundle）的自定义标题；空串时公开页回退
-- 「打包分享（N 项）」。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
