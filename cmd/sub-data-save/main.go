package main

import (
	"github.com/jzg-lab/sub-data-save/internal/plugin"
	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

func main() { pluginv1.Serve(plugin.New()) }
