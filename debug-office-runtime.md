# Office 运行时诊断记录

状态：[OPEN]
会话：office-runtime

## 可证伪假设
1. ONLYOFFICE_PUBLIC_URL 或 Caddy 路由不可从 OnlyOffice 容器访问。
2. backend 与 OnlyOffice 的 JWT secret 或启用状态不一致。
3. 回源 URL 的文件名/扩展名或响应头不符合 OnlyOffice 预期。
4. 模板生成的 OOXML 不完整或损坏。
5. 历史损坏文件未被 backend 识别并返回明确错误。

## 证据

## 修复

## 回归验证
