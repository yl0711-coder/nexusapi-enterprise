// Package web 内嵌生产前端 SPA(index.html + app.css + app.js),由后端在 / 提供。
// 前端是静态资源,与 /api/v1 同进程同镜像,单一可部署产物(镜像方式部署)。
package web

import "embed"

// FS 内嵌前端静态文件。
//
//go:embed index.html app.css app.js
var FS embed.FS
