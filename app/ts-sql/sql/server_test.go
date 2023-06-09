/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingestserver

import (
	"net"
	"path"
	"testing"
	"time"

	"github.com/openGemini/openGemini/app"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/metaclient"
	"github.com/openGemini/openGemini/open_src/influx/query"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_NewServer(t *testing.T) {
	tmpDir := t.TempDir()

	log := logger.NewLogger(errno.ModuleUnknown)
	cmd := &cobra.Command{
		Version: "Version",
	}

	conf := config.NewTSSql()
	conf.Common.ReportEnable = false
	conf.Sherlock.DumpPath = path.Join(tmpDir, "sherlock")

	server, err := NewServer(conf, cmd, log)
	require.NoError(t, err)
	require.NotNil(t, server.(*Server).MetaClient)
	require.NotNil(t, server.(*Server).TSDBStore)
	require.NotNil(t, server.(*Server).castorService)
	require.NotNil(t, server.(*Server).sherlockService)
}

func Test_NewServer_Open_Close(t *testing.T) {
	tmpDir := t.TempDir()

	log := logger.NewLogger(errno.ModuleUnknown)
	cmd := &cobra.Command{
		ValidArgs: []string{"dev", "abcd", "now"},
		Version:   "Version",
	}

	conf := config.NewTSSql()
	conf.Common.MetaJoin = append(conf.Common.MetaJoin, []string{"127.0.0.1:9179"}...)
	conf.Common.ReportEnable = false
	conf.Sherlock.DumpPath = path.Join(tmpDir, "sherlock")

	server, err := NewServer(conf, cmd, log)
	require.NoError(t, err)
	require.NotNil(t, server.(*Server).sherlockService)

	server.(*Server).initMetaClientFn = func() error {
		return nil
	}
	err = server.Open()
	require.NoError(t, err)

	err = server.Close()
	require.NoError(t, err)
}

func TestServer_Close(t *testing.T) {
	var err error
	server := Server{}

	server.Listener, err = net.Listen("tcp", "127.0.0.3:8899")
	if !assert.NoError(t, err) {
		return
	}

	server.QueryExecutor = query.NewExecutor()
	server.MetaClient = metaclient.NewClient("", false, 100)

	assert.NoError(t, server.Close())
}

func TestInitStatisticsPusher(t *testing.T) {
	server := &Server{}
	server.Logger = logger.NewLogger(errno.ModuleUnknown)
	server.config = config.NewTSSql()
	server.config.Monitor.Pushers = "http"
	server.config.Monitor.StoreEnabled = true

	app.SwitchToSingle()
	server.initStatisticsPusher()
	time.Sleep(10 * time.Millisecond)
	server.Close()
}
