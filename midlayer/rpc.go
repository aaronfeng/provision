package midlayer

import (
	"fmt"
	"io"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os/exec"

	"github.com/digitalrebar/provision/backend"
)

type pipePair struct {
	reader io.ReadCloser
	writer io.WriteCloser
}

func (this *pipePair) Read(p []byte) (int, error) {
	return this.reader.Read(p)
}

func (this *pipePair) Write(p []byte) (int, error) {
	return this.writer.Write(p)
}

func (this *pipePair) Close() error {
	this.writer.Close()
	return this.reader.Close()
}

type PluginRpcClient struct {
	cmd       *exec.Cmd
	rpcClient *rpc.Client
}

func NewPluginRpcClient(path string, params map[string]interface{}) *PluginRpcClient {
	answer := &PluginRpcClient{}

	answer.cmd = exec.Command(path, "listen")
	in := pipePair{}
	in.reader, _ = answer.cmd.StdoutPipe()
	in.writer, _ = answer.cmd.StdinPipe()

	answer.rpcClient = jsonrpc.NewClient(&in)

	answer.cmd.Start()

	var err backend.Error
	terr := answer.rpcClient.Call("Plugin.Config", params, &err)
	if terr != nil {
		fmt.Printf("GREG: error = %v\n", terr)
	}
	if err.Code != 0 {
		fmt.Printf("GREG: error = %v\n", err)
	}

	return answer
}

func (prpc *PluginRpcClient) Publish(e *backend.Event) error {
	var err backend.Error
	e2 := prpc.rpcClient.Call("Plugin.Publish", *e, &err)
	return e2
}

func (prpc *PluginRpcClient) Stop() {

}
