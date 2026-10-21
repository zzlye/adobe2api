# 部署与更新

## 启动服务

下载仓库后，进入项目目录。Linux 服务器可以直接拉取发布镜像：

```bash
sudo install -d -m 700 -o 10001 -g 10001 ./data ./data/tmp ./data/home
docker compose -f compose.deploy.yaml pull
docker compose -f compose.deploy.yaml up -d
```

打开 `http://服务器地址:8080`，首次访问设置管理员账号与密码。在账号池导入 Adobe 登录信息，在设置中查看或更换调用密钥。

## 使用指定硬盘和端口

先确认数据盘已经挂载，再在项目目录创建 `.env`：

```dotenv
ADOBE2API_PORT=8081
ADOBE2API_DATA_PATH=/mnt/data200/adobe2api/data
```

准备数据目录后启动：

```bash
mountpoint -q /mnt/data200
sudo install -d -m 700 -o 10001 -g 10001 \
  /mnt/data200/adobe2api/data \
  /mnt/data200/adobe2api/data/tmp \
  /mnt/data200/adobe2api/data/home
docker compose -f compose.deploy.yaml pull
docker compose -f compose.deploy.yaml up -d
```

此时打开 `http://服务器地址:8081`。需要返回图片链接时，将设置中的图片访问地址填写为这个地址，或填写已配置好的域名地址。账号、设置、生成记录、图片和浏览器临时资料均保存在指定目录。

## 更新与备份

更新前备份数据目录，然后执行：

```bash
docker compose -f compose.deploy.yaml pull
docker compose -f compose.deploy.yaml up -d
```

通过 `docker compose -f compose.deploy.yaml ps` 检查服务，通过 `docker compose -f compose.deploy.yaml logs --tail=100` 查看运行情况。更新和重启不会删除数据目录。迁移时停止服务，完整复制数据目录，并保留目录的所属用户和权限。

图片与生成记录的保留期限在设置页面调整，填写 `0` 表示不按期限删除。按容量清理图片需同时检查占用上限设置。
