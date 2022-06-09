package main

import (
	"net/http"

	"go.seankhliao.com/earbug-gchat/server"
	"go.seankhliao.com/svcrunner"
)

func main() {
	hs := &http.Server{}
	svr := server.New(hs)
	svcrunner.Options{}.Run(
		svcrunner.NewHTTP(hs, svr.Register, svr.Init),
	)
}
