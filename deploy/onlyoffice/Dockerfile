# OnlyOffice DocumentServer + 中文字体：
# 官方镜像不含 CJK 字体，中文文档在查看/编辑与格式转换（x2t）中会渲染
# 为方块乱码（fc-list :lang=zh 为空）。叠加 fonts-noto-cjk 后 fontconfig
# 即可命中中文字形；presentationthemeeditor 字体表由部署后执行一次
# documentserver-generate-allfont 重建（结果落 onlyoffice_data 卷持久）。
# apt 源替换为阿里云镜像（国内网络直连 archive.ubuntu.com 间歇失败）。
FROM onlyoffice/documentserver:8.2.3

RUN sed -i 's|http://archive.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g; s|http://security.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g' /etc/apt/sources.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*
