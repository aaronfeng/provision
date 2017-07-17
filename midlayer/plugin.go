package midlayer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os/exec"
	"sort"
	"sync"

	"github.com/digitalrebar/provision/backend"
	"github.com/digitalrebar/provision/backend/index"
	"github.com/fsnotify/fsnotify"
)

// Plugin Provider describes the available functions that could be
// instantiated by a plugin.
// swagger:model
type PluginProvider struct {
	Name           string
	Version        string
	Interfaces     []string
	RequiredParams []*backend.Param
	OptionalParams []*backend.Param

	path string
}

type RunningPlugin struct {
	Plugin *backend.Plugin
	Client *PluginRpcClient
}

type PluginController struct {
	logger             *log.Logger
	lock               sync.Mutex
	AvailableProviders map[string]*PluginProvider
	runningPlugins     []*RunningPlugin
	dataTracker        *backend.DataTracker
	pluginDir          string
	watcher            *fsnotify.Watcher
	done               chan bool
	finished           chan bool
	events             chan *backend.Event
	publishers         *backend.Publishers
}

func InitPluginController(pluginDir string, dt *backend.DataTracker, logger *log.Logger, pubs *backend.Publishers) (pc *PluginController, err error) {
	pc = &PluginController{pluginDir: pluginDir, dataTracker: dt, publishers: pubs, logger: logger,
		AvailableProviders: make(map[string]*PluginProvider, 0)}

	pubs.Add(pc)

	pc.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return
	}

	err = pc.watcher.Add(pc.pluginDir)
	if err != nil {
		return
	}

	pc.done = make(chan bool)
	pc.finished = make(chan bool)
	pc.events = make(chan *backend.Event, 1000)

	go func() {
		done := false
		for !done {
			select {
			case event := <-pc.watcher.Events:
				pc.logger.Println("file event:", event)
				if event.Op&fsnotify.Write == fsnotify.Write ||
					event.Op&fsnotify.Create == fsnotify.Create ||
					event.Op&fsnotify.Chmod == fsnotify.Chmod {
					pc.lock.Lock()
					pc.importPluginProvider(event.Name)
					pc.lock.Unlock()
				} else if event.Op&fsnotify.Remove == fsnotify.Remove {
					pc.lock.Lock()
					pc.removePluginProvider(event.Name)
					pc.lock.Unlock()
				} else if event.Op&fsnotify.Rename == fsnotify.Rename {
					pc.logger.Printf("Rename file: %s %v\n", event.Name, event)
				} else {
					pc.logger.Println("Unhandled file event:", event.Name)
				}
			case event := <-pc.events:
				if event.Action == "create" {
					pc.lock.Lock()
					ref := dt.NewPlugin()
					d, unlocker := dt.LockEnts(ref.Locks("get")...)
					ref2 := d(ref.Prefix()).Find(event.Key)
					pc.startPlugin(d, ref2.(*backend.Plugin))
					unlocker()
					pc.lock.Unlock()
				} else if event.Action == "save" {
					pc.lock.Lock()
					ref := dt.NewPlugin()
					d, unlocker := dt.LockEnts(ref.Locks("get")...)
					ref2 := d(ref.Prefix()).Find(event.Key)
					pc.restartPlugin(d, ref2.(*backend.Plugin))
					unlocker()
					pc.lock.Unlock()
				} else if event.Action == "update" {
					pc.lock.Lock()
					ref := dt.NewPlugin()
					d, unlocker := dt.LockEnts(ref.Locks("get")...)
					ref2 := d(ref.Prefix()).Find(event.Key)
					pc.restartPlugin(d, ref2.(*backend.Plugin))
					unlocker()
					pc.lock.Unlock()
				} else if event.Action == "delete" {
					pc.lock.Lock()
					pc.stopPlugin(event.Object.(*backend.Plugin))
					pc.lock.Unlock()
				} else {
					pc.logger.Println("internal event:", event)
				}
			case err := <-pc.watcher.Errors:
				pc.logger.Println("error:", err)
			case <-pc.done:
				done = true
			}
		}
		pc.finished <- true
	}()

	pc.lock.Lock()
	defer pc.lock.Unlock()

	// Walk plugin dir contents with lock
	files, err := ioutil.ReadDir(pc.pluginDir)
	if err != nil {
		return
	}
	for _, f := range files {
		pc.importPluginProvider(f.Name())
	}

	// Walk all plugin objects from dt.
	var idx *index.Index
	ref := dt.NewPlugin()
	d, unlocker := dt.LockEnts(ref.Locks("get")...)
	defer unlocker()
	idx, err = index.All([]index.Filter{index.Native()}...)(&d(ref.Prefix()).Index)
	if err != nil {
		return
	}
	arr := idx.Items()
	for _, res := range arr {
		plugin := res.(*backend.Plugin)
		pc.startPlugin(d, plugin)
	}

	return
}

func (pc *PluginController) Shutdown(ctx context.Context) error {
	pc.done <- true
	<-pc.finished
	return pc.watcher.Close()
}

func (pc *PluginController) Publish(e *backend.Event) error {
	if e.Type != "plugins" {
		return nil
	}
	pc.events <- e
	return nil
}

func (pc *PluginController) GetPluginProvider(name string) *PluginProvider {
	pc.lock.Lock()
	defer pc.lock.Unlock()

	if pp, ok := pc.AvailableProviders[name]; !ok {
		return nil
	} else {
		return pp
	}
}

func (pc *PluginController) GetPluginProviders() []*PluginProvider {
	pc.lock.Lock()
	defer pc.lock.Unlock()

	// get the list of keys and sort them
	keys := []string{}
	for key := range pc.AvailableProviders {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	answer := []*PluginProvider{}
	for _, key := range keys {
		answer = append(answer, pc.AvailableProviders[key])
	}
	return answer
}

func (pc *PluginController) startPlugin(d backend.Stores, plugin *backend.Plugin) {
	pc.logger.Printf("Starting plugin: %s(%s)\n", plugin.Name, plugin.Provider)
	pp, ok := pc.AvailableProviders[plugin.Provider]
	if ok {
		errors := []string{}

		for _, rp := range pp.RequiredParams {
			obj, ok := plugin.Params[rp.Name]
			if !ok {
				errors = append(errors, fmt.Sprintf("Missing required parameter: %s", rp.Name))
			} else {
				if ev := rp.Validate(obj); ev != nil {
					errors = append(errors, ev.Error())
				}
			}
		}
		for _, rp := range pp.OptionalParams {
			obj, ok := plugin.Params[rp.Name]
			if ok {
				if ev := rp.Validate(obj); ev != nil {
					errors = append(errors, ev.Error())
				}
			}
		}

		if len(errors) == 0 {
			thingee := NewPluginRpcClient(pp.path, plugin.Params)
			pc.publishers.Add(thingee)
			pc.runningPlugins = append(pc.runningPlugins, &RunningPlugin{Plugin: plugin, Client: thingee})
		}

		if len(plugin.Errors) != len(errors) {
			plugin.Errors = errors
			pc.dataTracker.Update(d, plugin)
		}
	} else {
		plugin.Errors = []string{fmt.Sprintf("Missing Plugin Provider: %s", plugin.Provider)}
		pc.dataTracker.Update(d, plugin)
	}
}

func (pc *PluginController) stopPlugin(plugin *backend.Plugin) {
	// GREG: Stop a plugin!!!
}

func (pc *PluginController) restartPlugin(d backend.Stores, plugin *backend.Plugin) {
	pc.stopPlugin(plugin)
	pc.startPlugin(d, plugin)
}

// Try to add to available - Must lock before calling
func (pc *PluginController) importPluginProvider(provider string) {
	out, err := exec.Command(pc.pluginDir+"/"+provider, "define").Output()
	if err != nil {
		pc.logger.Printf("Skipping %s because %s\n", provider, err)
	} else {
		var pp PluginProvider
		err = json.Unmarshal(out, &pp)
		if err != nil {
			pc.logger.Printf("Skipping %s because of bad json: %s\n%s\n", provider, err, out)
		} else {
			pc.logger.Printf("Adding plugin: %s\n", pp.Name)

			skip := false
			for _, p := range pp.RequiredParams {
				err := p.BeforeSave()
				if err != nil {
					pc.logger.Printf("Skipping %s because of bad required scheme: %s %s\n", pp.Name, p.Name, err)
					skip = true
				}
			}
			for _, p := range pp.OptionalParams {
				err := p.BeforeSave()
				if err != nil {
					pc.logger.Printf("Skipping %s because of bad optional scheme: %s %s\n", pp.Name, p.Name, err)
					skip = true
				}
			}

			if !skip {
				pc.AvailableProviders[pp.Name] = &pp
				pp.path = pc.pluginDir + "/" + provider
			}
		}
	}
}

// Try to stop using plugins and remove available - Must lock before calling
func (pc *PluginController) removePluginProvider(provider string) {
	var name string
	for _, pp := range pc.AvailableProviders {
		if provider == pp.path {
			name = pp.Name
			// GREG: If a running provider has its executable deleted, should we clear the running
			// plugin instannces.  I'm not sure what will happen.
		}
	}
	if name != "" {
		delete(pc.AvailableProviders, name)
	}
}
