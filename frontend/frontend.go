package frontend

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/VictorLowther/jsonpatch2"
	"github.com/digitalrebar/digitalrebar/go/common/store"
	"github.com/digitalrebar/provision/backend"
	"github.com/digitalrebar/provision/backend/index"
	"github.com/digitalrebar/provision/embedded"
	"github.com/digitalrebar/provision/midlayer"
	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/location"
	"github.com/gin-gonic/gin"
	"gopkg.in/olahol/melody.v1"
)

// ErrorResponse is returned whenever an error occurs
// swagger:response
type ErrorResponse struct {
	//in: body
	Body backend.Error
}

// NoContentResponse is returned for deletes and auth errors
// swagger:response
type NoContentResponse struct {
	//description: Nothing
}

type Sanitizable interface {
	Sanitize() store.KeySaver
}

type Lockable interface {
	Locks(string) []string
}

type Frontend struct {
	Logger     *log.Logger
	FileRoot   string
	MgmtApi    *gin.Engine
	ApiGroup   *gin.RouterGroup
	dt         *backend.DataTracker
	pc         *midlayer.PluginController
	authSource AuthSource
	pubs       *backend.Publishers
	melody     *melody.Melody
}

type AuthSource interface {
	GetUser(username string) *backend.User
}

type DefaultAuthSource struct {
	dt *backend.DataTracker
}

func (d DefaultAuthSource) GetUser(username string) *backend.User {
	objs, unlocker := d.dt.LockEnts("users")
	defer unlocker()
	u := objs("users").Find(username)
	if u != nil {
		return backend.AsUser(u)
	}
	return nil
}

func NewDefaultAuthSource(dt *backend.DataTracker) (das AuthSource) {
	das = DefaultAuthSource{dt: dt}
	return
}

func NewFrontend(dt *backend.DataTracker, logger *log.Logger, address string, port int, fileRoot, devUI string, authSource AuthSource, pubs *backend.Publishers, pc *midlayer.PluginController) (me *Frontend) {
	gin.SetMode(gin.ReleaseMode)

	if authSource == nil {
		authSource = NewDefaultAuthSource(dt)
	}

	userAuth := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			authHeader := c.Request.Header.Get("Authorization")
			if len(authHeader) == 0 {
				authHeader = c.Query("token")
				if len(authHeader) == 0 {
					logger.Printf("No authentication header or token")
					c.Header("WWW-Authenticate", "dr-provision")
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				} else {
					if strings.Contains(authHeader, ":") {
						authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(authHeader))
					} else {
						authHeader = "Bearer " + authHeader
					}
				}
			}
			hdrParts := strings.SplitN(authHeader, " ", 2)
			if len(hdrParts) != 2 || (hdrParts[0] != "Basic" && hdrParts[0] != "Bearer") {
				logger.Printf("Bad auth header: %s", authHeader)
				c.Header("WWW-Authenticate", "dr-provision")
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			if hdrParts[0] == "Basic" {
				hdr, err := base64.StdEncoding.DecodeString(hdrParts[1])
				if err != nil {
					logger.Printf("Malformed basic auth string: %s", hdrParts[1])
					c.Header("WWW-Authenticate", "dr-provision")
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				userpass := bytes.SplitN(hdr, []byte(`:`), 2)
				if len(userpass) != 2 {
					logger.Printf("Malformed basic auth string: %s", hdrParts[1])
					c.Header("WWW-Authenticate", "dr-provision")
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				user := authSource.GetUser(string(userpass[0]))
				if user == nil {
					logger.Printf("No such user: %s", string(userpass[0]))
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
				if !user.CheckPassword(string(userpass[1])) {
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
				t := backend.NewClaim(string(userpass[0]), 30).Add("*", "*", "*")
				c.Set("DRP-CLAIM", t)
			} else if hdrParts[0] == "Bearer" {
				t, err := dt.GetToken(string(hdrParts[1]))
				if err != nil {
					logger.Printf("No DRP authentication token")
					c.Header("WWW-Authenticate", "dr-provision")
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
				c.Set("DRP-CLAIM", t)
			}

			c.Next()
		}
	}

	mgmtApi := gin.Default()

	// CORS Support
	mgmtApi.Use(cors.New(cors.Config{
		AllowAllOrigins:  true,
		AllowCredentials: true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH", "HEAD"},
		AllowHeaders:     []string{"Origin", "X-Requested-With", "Content-Type", "Cookie", "Authorization", "WWW-Authenticate", "X-Return-Attributes"},
		ExposeHeaders:    []string{"Content-Length", "WWW-Authenticate", "Set-Cookie", "Access-Control-Allow-Headers", "Access-Control-Allow-Credentials", "Access-Control-Allow-Origin", "X-Return-Attributes"},
	}))

	mgmtApi.Use(location.Default())

	apiGroup := mgmtApi.Group("/api/v3")
	apiGroup.Use(userAuth())

	me = &Frontend{Logger: logger, FileRoot: fileRoot, MgmtApi: mgmtApi, ApiGroup: apiGroup, dt: dt, pubs: pubs, pc: pc}

	me.InitWebSocket()
	me.InitBootEnvApi()
	me.InitIsoApi()
	me.InitFileApi()
	me.InitTemplateApi()
	me.InitMachineApi()
	me.InitProfileApi()
	me.InitLeaseApi()
	me.InitReservationApi()
	me.InitSubnetApi()
	me.InitUserApi()
	me.InitInterfaceApi()
	me.InitPrefApi()
	me.InitParamApi()
	me.InitPluginApi()
	me.InitPluginProviderApi()

	// Swagger.json serve
	buf, err := embedded.Asset("swagger.json")
	if err != nil {
		logger.Fatalf("Failed to load swagger.json asset")
	}
	var f interface{}
	err = json.Unmarshal(buf, &f)
	mgmtApi.GET("/swagger.json", func(c *gin.Context) {
		c.JSON(http.StatusOK, f)
	})

	// Server Swagger UI.
	mgmtApi.StaticFS("/swagger-ui",
		&assetfs.AssetFS{Asset: embedded.Asset, AssetDir: embedded.AssetDir, AssetInfo: embedded.AssetInfo, Prefix: "swagger-ui"})

	// Server UI with flag to run from local files instead of assets
	if len(devUI) == 0 {
		mgmtApi.StaticFS("/ui",
			&assetfs.AssetFS{Asset: embedded.Asset, AssetDir: embedded.AssetDir, AssetInfo: embedded.AssetInfo, Prefix: "ui/public"})
	} else {
		logger.Printf("DEV: Running UI from %s\n", devUI)
		mgmtApi.Static("/ui", devUI)
	}

	mgmtApi.GET("/ux", func(c *gin.Context) {
		incomingUrl := location.Get(c)

		url := fmt.Sprintf("https://rackn.github.io/provision-ux/#/e/%s", incomingUrl.Host)
		c.Redirect(http.StatusMovedPermanently, url)
	})

	// root path, forward to UI
	mgmtApi.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/ui/")
	})

	pubs.Add(me)

	return
}

func testContentType(c *gin.Context, ct string) bool {
	ct = strings.ToUpper(ct)
	test := strings.ToUpper(c.ContentType())

	return strings.Contains(test, ct)
}

func assureContentType(c *gin.Context, ct string) bool {
	if testContentType(c, ct) {
		return true
	}
	err := &backend.Error{Type: "API_ERROR", Code: http.StatusBadRequest}
	err.Errorf("Invalid content type: %s", c.ContentType())
	c.JSON(err.Code, err)
	return false
}

func assureAuth(c *gin.Context, logger *log.Logger, scope, action, specific string) bool {
	obj, ok := c.Get("DRP-CLAIM")
	if !ok {
		logger.Printf("Request with no claims\n")
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	drpClaim, ok := obj.(*backend.DrpCustomClaims)
	if !ok {
		logger.Printf("Request with bad claims\n")
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	if !drpClaim.Match(scope, action, specific) {
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	return true
}

func assureDecode(c *gin.Context, val interface{}) bool {
	if !assureContentType(c, "application/json") {
		return false
	}
	marshalErr := c.Bind(&val)
	if marshalErr == nil {
		return true
	}
	err := &backend.Error{Type: "API_ERROR", Code: http.StatusBadRequest}
	err.Merge(marshalErr)
	c.JSON(err.Code, err)
	return false
}

// This processes the value into a function, if function not specifed, assume Eq.
// Supported Forms:
//
//   Eq(value)
//   Lt(value)
//   Lte(value)
//   Gt(value)
//   Gte(value)
//   Ne(value)
//   Between(valueLower, valueHigher)
//   Except(valueLower, valueHigher)
//
func convertValueToFilter(v string) (index.Filter, error) {
	args := strings.SplitN(v, "(", 2)
	switch args[0] {
	case "Eq":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Eq(subargs[0]), nil
	case "Lt":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Lt(subargs[0]), nil
	case "Lte":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Lte(subargs[0]), nil
	case "Gt":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Gt(subargs[0]), nil
	case "Gte":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Gte(subargs[0]), nil
	case "Ne":
		subargs := strings.SplitN(args[1], ")", 2)
		return index.Ne(subargs[0]), nil
	case "Between":
		subargs := strings.SplitN(args[1], ")", 2)
		parts := strings.Split(subargs[0], ",")
		return index.Between(parts[0], parts[1]), nil
	case "Except":
		subargs := strings.SplitN(args[1], ")", 2)
		parts := strings.Split(subargs[0], ",")
		return index.Except(parts[0], parts[1]), nil
	default:
		return index.Eq(v), nil
	}
	return nil, fmt.Errorf("Should never get here")
}

func (f *Frontend) processFilters(ref store.KeySaver, params map[string][]string) ([]index.Filter, error) {
	filters := []index.Filter{}

	var indexes map[string]index.Maker
	if indexer, ok := ref.(index.Indexer); ok {
		indexes = indexer.Indexes()
	} else {
		indexes = map[string]index.Maker{}
	}

	for k, vs := range params {
		if k == "offset" || k == "limit" || k == "sort" || k == "reverse" {
			continue
		}

		if maker, ok := indexes[k]; ok {
			filters = append(filters, index.Sort(maker))
			subfilters := []index.Filter{}
			for _, v := range vs {
				f, err := convertValueToFilter(v)
				if err != nil {
					return nil, err
				}
				subfilters = append(subfilters, f)
			}
			filters = append(filters, index.Any(subfilters...))
		} else {
			return nil, fmt.Errorf("Filter not found: %s", k)
		}
	}

	if vs, ok := params["sort"]; ok {
		for _, piece := range vs {
			if maker, ok := indexes[piece]; ok {
				filters = append(filters, index.Sort(maker))
			} else {
				return nil, fmt.Errorf("Not sortable: %s", piece)
			}
		}
	} else {
		filters = append(filters, index.Native())
	}

	if _, ok := params["reverse"]; ok {
		filters = append(filters, index.Reverse())
	}

	// offset and limit must be last
	if vs, ok := params["offset"]; ok {
		num, err := strconv.Atoi(vs[0])
		if err == nil {
			filters = append(filters, index.Offset(num))
		} else {
			return nil, fmt.Errorf("Offset not valid: %v", err)
		}
	}
	if vs, ok := params["limit"]; ok {
		num, err := strconv.Atoi(vs[0])
		if err == nil {
			filters = append(filters, index.Limit(num))
		} else {
			return nil, fmt.Errorf("Limit not valid: %v", err)
		}
	}

	return filters, nil
}

func (f *Frontend) List(c *gin.Context, ref store.KeySaver) {
	if !assureAuth(c, f.Logger, ref.Prefix(), "list", "") {
		return
	}
	res := &backend.Error{
		Code:  http.StatusNotAcceptable,
		Type:  "API_ERROR",
		Model: ref.Prefix(),
	}
	filters, err := f.processFilters(ref, c.Request.URL.Query())
	if err != nil {
		res.Merge(err)
		c.JSON(res.Code, res)
		return
	}
	var idx *index.Index
	func() {
		d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("get")...)
		defer unlocker()
		idx, err = index.All(filters...)(&d(ref.Prefix()).Index)
	}()
	if err != nil {
		res.Merge(err)
		c.JSON(res.Code, res)
		return
	}
	arr := idx.Items()
	for i, res := range arr {
		s, ok := res.(Sanitizable)
		if ok {
			arr[i] = s.Sanitize()
		}
	}
	c.JSON(http.StatusOK, arr)
}

func (f *Frontend) Fetch(c *gin.Context, ref store.KeySaver, key string) {
	prefix := ref.Prefix()
	var err error
	func() {
		d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("get")...)
		defer unlocker()
		objs := d(prefix)
		idxer, ok := ref.(index.Indexer)
		found := false
		if ok {
			for idxName, idx := range idxer.Indexes() {
				idxKey := strings.TrimPrefix(key, idxName+":")
				if key == idxKey {
					continue
				}
				found = true
				ref = nil
				if !idx.Unique {
					break
				}
				items, err := index.All(index.Sort(idx))(&objs.Index)
				if err == nil {
					ref = items.Find(idxKey)
				}
				break
			}
		}
		if !found {
			ref = objs.Find(key)
		}
	}()
	if ref != nil {
		// TODO: This should really be done before the fetch - it may have issue with HexAddr-based things.
		if !assureAuth(c, f.Logger, prefix, "get", ref.Key()) {
			return
		}
		s, ok := ref.(Sanitizable)
		if ok {
			ref = s.Sanitize()
		}
		c.JSON(http.StatusOK, ref)
	} else {
		rerr := &backend.Error{
			Code:  http.StatusNotFound,
			Type:  "API_ERROR",
			Model: prefix,
			Key:   key,
		}
		estring := ""
		if err != nil {
			estring = err.Error()
		}
		rerr.Errorf("%s GET: %s: Not Found%s", rerr.Model, rerr.Key, estring)
		c.JSON(rerr.Code, rerr)
	}
}

func (f *Frontend) Create(c *gin.Context, val store.KeySaver) {
	if !assureDecode(c, val) {
		return
	}
	if !assureAuth(c, f.Logger, val.Prefix(), "create", "") {
		return
	}
	var err error
	func() {
		d, unlocker := f.dt.LockEnts(val.(Lockable).Locks("create")...)
		defer unlocker()
		_, err = f.dt.Create(d, val)
	}()
	if err != nil {
		be, ok := err.(*backend.Error)
		if ok {
			c.JSON(be.Code, be)
		} else {
			c.JSON(http.StatusBadRequest, backend.NewError("API_ERROR", http.StatusBadRequest, err.Error()))
		}
	} else {
		s, ok := val.(Sanitizable)
		if ok {
			val = s.Sanitize()
		}
		c.JSON(http.StatusCreated, val)
	}
}

func (f *Frontend) Patch(c *gin.Context, ref store.KeySaver, key string) {
	patch := make(jsonpatch2.Patch, 0)
	if !assureDecode(c, &patch) {
		return
	}
	if !assureAuth(c, f.Logger, ref.Prefix(), "patch", key) {
		return
	}
	var err error
	var res store.KeySaver
	func() {
		d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("update")...)
		defer unlocker()
		res, err = f.dt.Patch(d, ref, key, patch)
	}()
	if err == nil {
		s, ok := res.(Sanitizable)
		if ok {
			res = s.Sanitize()
		}
		c.JSON(http.StatusOK, res)
		return
	}
	ne, ok := err.(*backend.Error)
	if ok {
		c.JSON(ne.Code, ne)
	} else {
		c.JSON(http.StatusBadRequest, backend.NewError("API_ERROR", http.StatusBadRequest, err.Error()))
	}
}

func (f *Frontend) Update(c *gin.Context, ref store.KeySaver, key string) {
	if !assureDecode(c, ref) {
		return
	}
	if !assureAuth(c, f.Logger, ref.Prefix(), "update", key) {
		return
	}
	if ref.Key() != key {
		err := &backend.Error{
			Code:  http.StatusBadRequest,
			Type:  "API_ERROR",
			Model: ref.Prefix(),
			Key:   key,
		}
		err.Errorf("%s PUT: Key change from %s to %s not allowed", err.Model, key, ref.Key())
		c.JSON(err.Code, err)
		return
	}
	var err error
	func() {
		d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("update")...)
		defer unlocker()
		_, err = f.dt.Update(d, ref)
	}()
	if err == nil {
		s, ok := ref.(Sanitizable)
		if ok {
			ref = s.Sanitize()
		}
		c.JSON(http.StatusOK, ref)
		return
	}
	ne, ok := err.(*backend.Error)
	if ok {
		c.JSON(ne.Code, ne)
	} else {
		c.JSON(http.StatusBadRequest, backend.NewError("API_ERROR", http.StatusBadRequest, err.Error()))
	}
}

func (f *Frontend) Remove(c *gin.Context, ref store.KeySaver) {
	if !assureAuth(c, f.Logger, ref.Prefix(), "delete", ref.Key()) {
		return
	}
	var err error
	func() {
		d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("delete")...)
		defer unlocker()
		_, err = f.dt.Remove(d, ref)
	}()
	if err != nil {
		ne, ok := err.(*backend.Error)
		if ok {
			c.JSON(ne.Code, ne)
		} else {
			c.JSON(http.StatusNotFound, backend.NewError("API_ERROR", http.StatusBadRequest, err.Error()))
		}
	} else {
		s, ok := ref.(Sanitizable)
		if ok {
			ref = s.Sanitize()
		}
		c.JSON(http.StatusOK, ref)
	}
}
