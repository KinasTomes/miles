package main

import (
	"flag"
	"fmt"

	"github.com/KinasTomes/miles/internal/buildinfo"
)

func main() {
	showVersion := flag.Bool("version", false, "Hiển thị phiên bản")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "CLI quản trị Miles")
		fmt.Fprintln(flag.CommandLine.Output(), "Skeleton dự án; chức năng ứng dụng chưa được triển khai.")
		fmt.Fprintln(flag.CommandLine.Output(), "\nTùy chọn:")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.String("dsgctl"))
		return
	}
	flag.Usage()
}
