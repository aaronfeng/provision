package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"text/template"

	"github.com/VictorLowther/jsonpatch2"
	"github.com/dgrijalva/jwt-go"
	"github.com/digitalrebar/digitalrebar/go/common/store"
	"github.com/digitalrebar/provision/backend/index"
)

var (
	ignoreBoot = &BootEnv{
		Name:        "ignore",
		Description: "The boot environment you should use to have unknown machines boot off their local hard drive",
		OS:          OsInfo{Name: "ignore"},
		OnlyUnknown: true,
		Templates: []TemplateInfo{
			{
				Name: "pxelinux",
				Path: "pxelinux.cfg/default",
				Contents: `DEFAULT local
PROMPT 0
TIMEOUT 10
LABEL local
localboot 0
`,
			},
			{
				Name:     "elilo",
				Path:     "elilo.conf",
				Contents: "exit",
			},
			{
				Name: "ipxe",
				Path: "default.ipxe",
				Contents: `#!ipxe
chain tftp://{{.ProvisionerAddress}}/${netX/ip}.ipxe || exit
`,
			},
		},
	}
)

type followUpSaver interface {
	followUpSave()
}

type followUpDeleter interface {
	followUpDelete()
}

// dtobjs is an in-memory cache of all the objects we could
// reference. The implementation of this may need to change from
// storing a slice of things to a more elaborate datastructure at some
// point in time.  Since that point in time is when the slices are
// forced out of CPU cache, I am not terribly concerned for now.
// Until that point is reached, sorting and searching slices is
// fantastically efficient.
type Store struct {
	sync.Mutex
	index.Index
	backingStore store.SimpleStore
}

func (s *Store) getBackend(obj store.KeySaver) store.SimpleStore {
	return s.backingStore
}

type dtSetter interface {
	store.KeySaver
	setDT(*DataTracker)
}

// DataTracker represents everything there is to know about acting as
// a dataTracker.
type DataTracker struct {
	FileRoot            string
	OurAddress          string
	StaticPort, ApiPort int
	Logger              *log.Logger
	FS                  *FileSystem
	objs                map[string]*Store
	defaultPrefs        map[string]string
	runningPrefs        map[string]string
	prefMux             *sync.Mutex
	defaultBootEnv      string
	globalProfileName   string
	tokenManager        *JwtManager
	rootTemplate        *template.Template
	tmplMux             *sync.Mutex
	thunks              []func()
	thunkMux            *sync.Mutex
	publishers          *Publishers
}

type Stores func(string) *Store

// LockEnts grabs the requested Store locks a consistent order.
// It returns a function to get an Index that was requested, and
// a function that unlocks the taken locks in the right order.
func (p *DataTracker) LockEnts(ents ...string) (stores Stores, unlocker func()) {
	sortedEnts := make([]string, len(ents))
	copy(sortedEnts, ents)
	s := sort.StringSlice(sortedEnts)
	sort.Sort(sort.Reverse(s))
	sortedRes := map[string]*Store{}
	for _, ent := range s {
		objs, ok := p.objs[ent]
		if !ok {
			log.Panicf("Tried to reference nonexistent object type '%s'", ent)
		}
		sortedRes[ent] = objs
	}
	for _, ent := range s {
		sortedRes[ent].Lock()
	}
	srMux := &sync.Mutex{}
	return func(ref string) *Store {
			srMux.Lock()
			idx, ok := sortedRes[ref]
			srMux.Unlock()
			if !ok {
				log.Panicf("Tried to access unlocked resource %s", ref)
			}
			return idx
		},
		func() {
			srMux.Lock()
			for i := len(s) - 1; i >= 0; i-- {
				sortedRes[s[i]].Unlock()
				delete(sortedRes, s[i])
			}
			srMux.Unlock()
		}
}

func (p *DataTracker) LocalIP(remote net.IP) string {
	if localIP := LocalFor(remote); localIP != nil {
		return localIP.String()
	}
	// Determining what this is needs to be made smarter, probably by
	// firguing out which interface the default route goes over for ipv4
	// then ipv6, and then figurig out the appropriate address on that
	// interface
	return p.OurAddress
}

func (p *DataTracker) urlFor(scheme string, remoteIP net.IP, port int) string {
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(p.LocalIP(remoteIP), strconv.Itoa(port)))
}

func (p *DataTracker) FileURL(remoteIP net.IP) string {
	return p.urlFor("http", remoteIP, p.StaticPort)
}

func (p *DataTracker) ApiURL(remoteIP net.IP) string {
	return p.urlFor("https", remoteIP, p.ApiPort)
}

// Create a new DataTracker that will use passed store to save all operational data
func NewDataTracker(backend store.SimpleStore,
	fileRoot, addr string,
	staticPort, apiPort int,
	logger *log.Logger,
	defaultPrefs map[string]string,
	publishers *Publishers) *DataTracker {
	res := &DataTracker{
		FileRoot:          fileRoot,
		StaticPort:        staticPort,
		ApiPort:           apiPort,
		OurAddress:        addr,
		Logger:            logger,
		defaultPrefs:      defaultPrefs,
		runningPrefs:      map[string]string{},
		prefMux:           &sync.Mutex{},
		FS:                NewFS(fileRoot, logger),
		tokenManager:      NewJwtManager([]byte(randString(32)), JwtConfig{Method: jwt.SigningMethodHS256}),
		tmplMux:           &sync.Mutex{},
		globalProfileName: "global",
		thunks:            make([]func(), 0),
		thunkMux:          &sync.Mutex{},
		publishers:        publishers,
	}
	objs := []store.KeySaver{
		&Task{p: res},
		&Param{p: res},
		&Profile{p: res},
		&User{p: res},
		&Template{p: res},
		&BootEnv{p: res},
		&Machine{p: res},
		&Subnet{p: res},
		&Reservation{p: res},
		&Lease{p: res},
		&Pref{p: res},
		&Plugin{p: res},
	}
	res.objs = map[string]*Store{}
	for _, obj := range objs {
		prefix := obj.Prefix()
		bk, err := backend.Sub(prefix)
		if err != nil {
			res.Logger.Fatalf("dataTracker: Error creating substore %s: %v", prefix, err)
		}
		res.objs[prefix] = &Store{backingStore: bk}
		storeObjs, err := store.List(obj)
		if err != nil {
			res.Logger.Fatalf("dataTracker: Error loading data for %s: %v", prefix, err)
		}
		res.objs[prefix].Index = *index.Create(storeObjs)
		if obj.Prefix() == "templates" {
			buf := &bytes.Buffer{}
			for _, thing := range res.objs["templates"].Items() {
				tmpl := AsTemplate(thing)
				fmt.Fprintf(buf, `{{define "%s"}}%s{{end}}`, tmpl.ID, tmpl.Contents)
			}
			root, err := template.New("").Parse(buf.String())
			if err != nil {
				logger.Fatalf("Unable to load root templates: %v", err)
			}
			res.rootTemplate = root
			res.rootTemplate.Option("missingkey=error")
		}
	}
	d, unlocker := res.LockEnts("bootenvs", "preferences", "users", "machines", "profiles", "params")
	defer unlocker()
	if d("bootenvs").Find(ignoreBoot.Key()) == nil {
		res.Create(d, ignoreBoot)
	}
	for _, prefIsh := range d("preferences").Items() {
		pref := AsPref(prefIsh)
		res.runningPrefs[pref.Name] = pref.Val
	}
	if d("preferences").Find(res.globalProfileName) == nil {
		gp := AsProfile(res.NewProfile())
		gp.Name = "global"
		res.Create(d, gp)
	}
	users := d("users")
	if users.Count() == 0 {
		res.Infof("debugBootEnv", "Creating rocketskates user")
		user := &User{p: res, Name: "rocketskates"}
		if err := user.ChangePassword(d, "r0cketsk8ts"); err != nil {
			logger.Fatalf("Failed to create rocketskates user: %v", err)
		}
		res.Create(d, user)
	}
	res.defaultBootEnv = defaultPrefs["defaultBootEnv"]
	machines := d("machines")
	for _, obj := range machines.Items() {
		machine := AsMachine(obj)
		bootEnv := d("bootenvs").Find(machine.BootEnv)
		if bootEnv == nil {
			continue
		}
		err := &Error{o: machine}
		AsBootEnv(bootEnv).Render(d, machine, err).register(res.FS)
		if err.containsError {
			logger.Printf("Error rendering machine %s at startup:", machine.UUID())
			logger.Println(err.Error())
		}
	}
	if err := res.RenderUnknown(d); err != nil {
		logger.Fatalf("Failed to render unknown bootenv: %v", err)
	}
	return res
}

func (p *DataTracker) Prefs() map[string]string {
	vals := map[string]string{}
	p.prefMux.Lock()
	for k, v := range p.defaultPrefs {
		vals[k] = v
	}
	for k, v := range p.runningPrefs {
		vals[k] = v
	}
	p.prefMux.Unlock()
	return vals
}

func (p *DataTracker) Pref(name string) (string, error) {
	res, ok := p.Prefs()[name]
	if !ok {
		return "", fmt.Errorf("No such preference %s", name)
	}
	return res, nil
}

func (p *DataTracker) pref(name string) string {
	return p.Prefs()[name]
}

func (p *DataTracker) SetPrefs(d Stores, prefs map[string]string) error {
	err := &Error{}
	bootenvs := d("bootenvs")
	benvCheck := func(name, val string) *BootEnv {
		be := bootenvs.Find(val)
		if be == nil {
			err.Errorf("%s: Bootenv %s does not exist", name, val)
			return nil
		}
		return AsBootEnv(be)
	}
	intCheck := func(name, val string) bool {
		_, e := strconv.Atoi(val)
		if e == nil {
			return true
		}
		err.Errorf("%s: %s", name, e.Error())
		return false
	}
	savePref := func(name, val string) bool {
		p.prefMux.Lock()
		defer p.prefMux.Unlock()
		pref := &Pref{p: p, Name: name, Val: val}
		if _, saveErr := p.Save(d, pref); saveErr != nil {
			err.Errorf("%s: Failed to save %s: %v", name, val, saveErr)
			return false
		}
		p.runningPrefs[name] = val
		return true
	}
	for name, val := range prefs {
		switch name {
		case "defaultBootEnv":
			be := benvCheck(name, val)
			if be != nil && !be.OnlyUnknown {
				savePref(name, val)
				p.defaultBootEnv = val
			}
		case "unknownBootEnv":
			if benvCheck(name, val) != nil && savePref(name, val) {
				err.Merge(p.RenderUnknown(d))
			}
		case "unknownTokenTimeout",
			"knownTokenTimeout",
			"debugDhcp",
			"debugRenderer",
			"debugBootEnv":
			if intCheck(name, val) {
				savePref(name, val)
			}
			continue
		default:
			err.Errorf("Unknown preference %s", name)
		}
	}
	return err.OrNil()
}

func (p *DataTracker) RenderUnknown(d Stores) error {
	pref, e := p.Pref("unknownBootEnv")
	if e != nil {
		return e
	}
	envIsh := d("bootenvs").Find(pref)
	if envIsh == nil {
		return fmt.Errorf("No such BootEnv: %s", pref)
	}
	env := AsBootEnv(envIsh)
	err := &Error{o: env, Type: "StartupError"}
	if !env.Available {
		err.Messages = env.Errors
		err.containsError = true
		return err
	}
	if !env.OnlyUnknown {
		err.Errorf("BootEnv %s cannot be used for the unknownBootEnv", env.Name)
		return err
	}
	env.Render(d, nil, err).register(p.FS)
	return err.OrNil()
}

func (p *DataTracker) getBackend(t store.KeySaver) store.SimpleStore {
	res, ok := p.objs[t.Prefix()]
	if !ok {
		p.Logger.Fatalf("%s: No registered storage backend!", t.Prefix())
	}
	return res.backingStore
}

func (p *DataTracker) setDT(s store.KeySaver) {
	if tgt, ok := s.(dtSetter); ok {
		tgt.setDT(p)
	}
}

func (p *DataTracker) Clone(ref store.KeySaver) store.KeySaver {
	var res store.KeySaver
	switch ref.(type) {
	case *Machine:
		res = p.NewMachine()
	case *Param:
		res = p.NewParam()
	case *Profile:
		res = p.NewProfile()
	case *User:
		res = p.NewUser()
	case *Template:
		res = p.NewTemplate()
	case *BootEnv:
		res = p.NewBootEnv()
	case *Lease:
		res = p.NewLease()
	case *Reservation:
		res = p.NewReservation()
	case *Subnet:
		res = p.NewSubnet()
	case *Pref:
		res = p.NewPref()
	case *Task:
		res = p.NewTask()
	default:
		panic("Unknown type of KeySaver passed to Clone")
	}
	buf, err := json.Marshal(ref)
	if err != nil {
		panic(err.Error())
	}
	if err := json.Unmarshal(buf, &res); err != nil {
		panic(err.Error())
	}
	return res
}

func (p *DataTracker) Create(d Stores, ref store.KeySaver) (saved bool, err error) {
	p.setDT(ref)
	prefix := ref.Prefix()
	key := ref.Key()
	if key == "" {
		return false, fmt.Errorf("dataTracker create %s: Empty key not allowed", prefix)
	}
	if d(prefix).Find(key) != nil {
		return false, fmt.Errorf("dataTracker create %s: %s already exists", prefix, key)
	}
	ref.(validator).setStores(d)
	saved, err = store.Create(ref)
	if saved {
		ref.(validator).clearStores()
		d(prefix).Add(ref)

		p.publishers.Publish(prefix, "create", key, ref)
	}

	return saved, err
}

func (p *DataTracker) Remove(d Stores, ref store.KeySaver) (removed bool, err error) {
	prefix := ref.Prefix()
	key := ref.Key()
	item := d(prefix).Find(key)
	if item == nil {
		err := &Error{
			Code:  http.StatusNotFound,
			Key:   key,
			Model: prefix,
		}
		err.Errorf("%s: DELETE %s: Not Found", err.Model, err.Key)
		return false, err
	}
	item.(validator).setStores(d)
	removed, err = store.Remove(item)
	if removed {
		d(prefix).Remove(item)
		p.publishers.Publish(prefix, "delete", key, item)
	}
	return removed, err
}

func (p *DataTracker) Patch(d Stores, ref store.KeySaver, key string, patch jsonpatch2.Patch) (store.KeySaver, error) {
	prefix := ref.Prefix()
	target := d(prefix).Find(key)
	if target == nil {
		err := &Error{
			Code:  http.StatusNotFound,
			Key:   key,
			Model: prefix,
		}
		err.Errorf("%s: PATCH %s: Not Found", err.Model, err.Key)
		return nil, err
	}
	buf, fatalErr := json.Marshal(target)
	if fatalErr != nil {
		p.Logger.Fatalf("Non-JSON encodable %v:%v stored in cache: %v", prefix, key, fatalErr)
	}
	resBuf, patchErr, loc := patch.Apply(buf)
	if patchErr != nil {
		err := &Error{
			Code:  http.StatusNotAcceptable,
			Key:   key,
			Model: ref.Prefix(),
			Type:  "JsonPatchError",
		}
		err.Errorf("Patch error at line %d: %v", loc, patchErr)
		return nil, err
	}
	toSave := ref.New()
	if err := json.Unmarshal(resBuf, &toSave); err != nil {
		return nil, err
	}
	p.setDT(toSave)
	toSave.(validator).setStores(d)
	saved, err := store.Update(toSave)
	toSave.(validator).clearStores()
	if !saved {
		return toSave, err
	}
	d(prefix).Add(toSave)
	p.publishers.Publish(prefix, "update", key, ref)
	return toSave, nil
}

func (p *DataTracker) Update(d Stores, ref store.KeySaver) (saved bool, err error) {
	prefix := ref.Prefix()
	key := ref.Key()
	if d(prefix).Find(key) == nil {
		err := &Error{
			Code:  http.StatusNotFound,
			Key:   key,
			Model: prefix,
		}
		err.Errorf("%s: PUT %s: Not Found", err.Model, err.Key)
		return false, err
	}
	p.setDT(ref)
	ref.(validator).setStores(d)
	saved, err = store.Update(ref)
	ref.(validator).clearStores()
	if saved {
		d(prefix).Add(ref)
		p.publishers.Publish(prefix, "update", key, ref)
	}
	return saved, err
}

func (p *DataTracker) Save(d Stores, ref store.KeySaver) (saved bool, err error) {
	p.setDT(ref)
	ref.(validator).setStores(d)
	saved, err = store.Save(ref)
	ref.(validator).clearStores()
	if saved {
		d(ref.Prefix()).Add(ref)
		p.publishers.Publish(ref.Prefix(), "save", ref.Key(), ref)
	}
	return saved, err
}

func (p *DataTracker) GetToken(tokenString string) (*DrpCustomClaims, error) {
	return p.tokenManager.get(tokenString)
}

func (p *DataTracker) SealClaims(claims *DrpCustomClaims) (string, error) {
	return claims.Seal(p.tokenManager)
}

func (p *DataTracker) Backup() ([]byte, error) {
	keys := make([]string, len(p.objs))
	for k := range p.objs {
		keys = append(keys, k)
	}
	_, unlocker := p.LockEnts(keys...)
	defer unlocker()
	res := map[string][]store.KeySaver{}
	for _, k := range keys {
		res[k] = p.objs[k].Items()
	}
	return json.Marshal(res)
}

func (p *DataTracker) NewToken(id string, ttl int, scope, action, specific string) (string, error) {
	return NewClaim(id, ttl).Add(scope, action, specific).Seal(p.tokenManager)
}

func (p *DataTracker) Printf(f string, args ...interface{}) {
	p.Logger.Printf(f, args...)
}

func (p *DataTracker) Infof(pref, f string, args ...interface{}) {
	debugLevel := 0
	d2, e := strconv.Atoi(p.pref(pref))
	if e == nil {
		debugLevel = d2
	}
	if debugLevel > 0 {
		p.Logger.Printf(f, args...)
	}
}
func (p *DataTracker) Debugf(pref, f string, args ...interface{}) {
	p.Infof(pref, f, args...)
}
