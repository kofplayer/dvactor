@echo off
rem 使用仓库内置 protoc 重新生成协议；protoc-gen-go 需在 PATH 中
rem （安装：go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6，
rem   版本需与 go.mod 的 protobuf 保持一致）
set PROTOC=%~dp0tools\protoc\bin\protoc.exe
"%PROTOC%" --go_out=. --go_opt=paths=source_relative .\protocol\cluster.proto
