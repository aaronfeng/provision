package frontend

import (
	"net/http"

	"github.com/VictorLowther/jsonpatch2"
	"github.com/digitalrebar/digitalrebar/go/common/store"
	"github.com/digitalrebar/provision/backend"
	"github.com/gin-gonic/gin"
)

// PluginResponse return on a successful GET, PUT, PATCH or POST of a single Plugin
// swagger:response
type PluginResponse struct {
	// in: body
	Body *backend.Plugin
}

// PluginsResponse return on a successful GET of all Plugins
// swagger:response
type PluginsResponse struct {
	// in: body
	Body []*backend.Plugin
}

// PluginParamsResponse return on a successful GET of all Plugin's Params
// swagger:response
type PluginParamsResponse struct {
	// in: body
	Body map[string]interface{}
}

// PluginBodyParameter used to inject a Plugin
// swagger:parameters createPlugin putPlugin
type PluginBodyParameter struct {
	// in: body
	// required: true
	Body *backend.Plugin
}

// PluginPatchBodyParameter used to patch a Plugin
// swagger:parameters patchPlugin
type PluginPatchBodyParameter struct {
	// in: body
	// required: true
	Body jsonpatch2.Patch
}

// PluginPathParameter used to find a Plugin in the path
// swagger:parameters putPlugins getPlugin putPlugin patchPlugin deletePlugin getPluginParams postPluginParams
type PluginPathParameter struct {
	// in: path
	// required: true
	Name string `json:"name"`
}

// PluginParamsBodyParameter used to set Plugin Params
// swagger:parameters postPluginParams
type PluginParamsBodyParameter struct {
	// in: body
	// required: true
	Body map[string]interface{}
}

// PluginListPathParameter used to limit lists of Plugin by path options
// swagger:parameters listPlugins
type PluginListPathParameter struct {
	// in: query
	Offest int `json:"offset"`
	// in: query
	Limit int `json:"limit"`
	// in: query
	Name string
}

func (f *Frontend) InitPluginApi() {
	// swagger:route GET /plugins Plugins listPlugins
	//
	// Lists Plugins filtered by some parameters.
	//
	// This will show all Plugins by default.
	//
	// You may specify:
	//    Offset = integer, 0-based inclusive starting point in filter data.
	//    Limit = integer, number of items to return
	//
	// Functional Indexs:
	//    Name = string
	//    Provider = string
	//
	// Functions:
	//    Eq(value) = Return items that are equal to value
	//    Lt(value) = Return items that are less than value
	//    Lte(value) = Return items that less than or equal to value
	//    Gt(value) = Return items that are greater than value
	//    Gte(value) = Return items that greater than or equal to value
	//    Between(lower,upper) = Return items that are inclusively between lower and upper
	//    Except(lower,upper) = Return items that are not inclusively between lower and upper
	//
	// Example:
	//    Name=fred - returns items named fred
	//    Name=Lt(fred) - returns items that alphabetically less than fred.
	//    Name=Lt(fred)&Available=true - returns items with Name less than fred and Available is true
	//
	// Responses:
	//    200: PluginsResponse
	//    401: NoContentResponse
	//    403: NoContentResponse
	//    406: ErrorResponse
	f.ApiGroup.GET("/plugins",
		func(c *gin.Context) {
			f.List(c, f.dt.NewPlugin())
		})

	// swagger:route POST /plugins Plugins createPlugin
	//
	// Create a Plugin
	//
	// Create a Plugin from the provided object
	//
	//     Responses:
	//       201: PluginResponse
	//       400: ErrorResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       422: ErrorResponse
	f.ApiGroup.POST("/plugins",
		func(c *gin.Context) {
			// We don't use f.Create() because we need to be able to assign random
			// UUIDs to new Plugins without forcing the client to do so, yet allow them
			// for testing purposes amd if they alrady have a UUID scheme for plugins.
			b := f.dt.NewPlugin()
			if !assureDecode(c, b) {
				return
			}
			var res store.KeySaver
			var err error
			func() {
				d, unlocker := f.dt.LockEnts(store.KeySaver(b).(Lockable).Locks("create")...)
				defer unlocker()
				_, err = f.dt.Create(d, b)
			}()
			if err != nil {
				be, ok := err.(*backend.Error)
				if ok {
					c.JSON(be.Code, be)
				} else {
					c.JSON(http.StatusBadRequest, backend.NewError("API_ERROR", http.StatusBadRequest, err.Error()))
				}
			} else {
				s, ok := store.KeySaver(b).(Sanitizable)
				if ok {
					res = s.Sanitize()
				} else {
					res = b
				}
				c.JSON(http.StatusCreated, res)
			}
		})

	// swagger:route GET /plugins/{name} Plugins getPlugin
	//
	// Get a Plugin
	//
	// Get the Plugin specified by {name} or return NotFound.
	//
	//     Responses:
	//       200: PluginResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	f.ApiGroup.GET("/plugins/:name",
		func(c *gin.Context) {
			f.Fetch(c, f.dt.NewPlugin(), c.Param(`name`))
		})

	// swagger:route PATCH /plugins/{name} Plugins patchPlugin
	//
	// Patch a Plugin
	//
	// Update a Plugin specified by {name} using a RFC6902 Patch structure
	//
	//     Responses:
	//       200: PluginResponse
	//       400: ErrorResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	//       406: ErrorResponse
	//       422: ErrorResponse
	f.ApiGroup.PATCH("/plugins/:name",
		func(c *gin.Context) {
			f.Patch(c, f.dt.NewPlugin(), c.Param(`name`))
		})

	// swagger:route PUT /plugins/{name} Plugins putPlugin
	//
	// Put a Plugin
	//
	// Update a Plugin specified by {name} using a JSON Plugin
	//
	//     Responses:
	//       200: PluginResponse
	//       400: ErrorResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	//       422: ErrorResponse
	f.ApiGroup.PUT("/plugins/:name",
		func(c *gin.Context) {
			f.Update(c, f.dt.NewPlugin(), c.Param(`name`))
		})

	// swagger:route DELETE /plugins/{name} Plugins deletePlugin
	//
	// Delete a Plugin
	//
	// Delete a Plugin specified by {name}
	//
	//     Responses:
	//       200: PluginResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	f.ApiGroup.DELETE("/plugins/:name",
		func(c *gin.Context) {
			b := f.dt.NewPlugin()
			b.Name = c.Param(`Name`)
			f.Remove(c, b)
		})

	// swagger:route GET /plugins/{name}/params Plugins getPluginParams
	//
	// List plugin params Plugin
	//
	// List Plugin parms for a Plugin specified by {name}
	//
	//     Responses:
	//       200: PluginParamsResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	f.ApiGroup.GET("/plugins/:name/params",
		func(c *gin.Context) {
			name := c.Param(`name`)
			b := f.dt.NewPlugin()
			var ref store.KeySaver
			func() {
				d, unlocker := f.dt.LockEnts(store.KeySaver(b).(Lockable).Locks("get")...)
				defer unlocker()
				ref = d("plugins").Find(name)
			}()
			if ref == nil {
				err := &backend.Error{
					Code:  http.StatusNotFound,
					Type:  "API_ERROR",
					Model: "plugins",
					Key:   name,
				}
				err.Errorf("%s GET Params: %s: Not Found", err.Model, err.Key)
				c.JSON(err.Code, err)
				return
			}
			if !assureAuth(c, f.Logger, ref.Prefix(), "get", ref.Key()) {
				return
			}
			p := backend.AsPlugin(ref).GetParams()
			c.JSON(http.StatusOK, p)
		})

	// swagger:route POST /plugins/{name}/params Plugins postPluginParams
	//
	// Set/Replace all the Parameters for a plugin specified by {name}
	//
	//     Responses:
	//       200: PluginParamsResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	//       409: ErrorResponse
	f.ApiGroup.POST("/plugins/:name/params",
		func(c *gin.Context) {
			var val map[string]interface{}
			if !assureDecode(c, &val) {
				return
			}
			name := c.Param(`name`)
			b := f.dt.NewPlugin()
			var ref store.KeySaver
			func() {
				d, unlocker := f.dt.LockEnts(store.KeySaver(b).(Lockable).Locks("get")...)
				defer unlocker()
				ref = d("plugins").Find(name)
			}()
			if ref == nil {
				err := &backend.Error{
					Code:  http.StatusNotFound,
					Type:  "API_ERROR",
					Model: "plugins",
					Key:   name,
				}
				err.Errorf("%s SET Params: %s: Not Found", err.Model, err.Key)
				c.JSON(err.Code, err)
				return
			}
			if !assureAuth(c, f.Logger, ref.Prefix(), "get", ref.Key()) {
				return
			}

			m := backend.AsPlugin(ref)
			var err error
			func() {
				d, unlocker := f.dt.LockEnts(ref.(Lockable).Locks("update")...)
				defer unlocker()
				err = m.SetParams(d, val)
			}()
			if err != nil {
				be, _ := err.(*backend.Error)
				c.JSON(be.Code, be)
			} else {
				c.JSON(http.StatusOK, val)
			}
		})

}
