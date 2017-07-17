package frontend

import (
	"fmt"
	"net/http"

	"github.com/digitalrebar/provision/backend"
	"github.com/digitalrebar/provision/midlayer"
	"github.com/gin-gonic/gin"
)

// PluginProvidersResponse returned on a successful GET of an plugin_provider
// swagger:response
type PluginProviderResponse struct {
	// in: body
	Body *midlayer.PluginProvider
}

// PluginProvidersResponse returned on a successful GET of all plugin_provider
// swagger:response
type PluginProvidersResponse struct {
	// in: body
	Body []*midlayer.PluginProvider
}

// swagger:parameters getPluginProvider
type PluginProviderParameter struct {
	// in: path
	Name string `json:"name"`
}

func (f *Frontend) InitPluginProviderApi() {
	// swagger:route GET /plugin_providers PluginProviders listPluginProviders
	//
	// Lists possible plugin_provider on the system to create plugins
	//
	//     Produces:
	//       application/json
	//
	//     Responses:
	//       200: PluginProvidersResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       500: ErrorResponse
	f.ApiGroup.GET("/plugin_providers",
		func(c *gin.Context) {
			if !assureAuth(c, f.Logger, "plugin_providers", "list", "") {
				return
			}
			c.JSON(http.StatusOK, f.pc.GetPluginProviders())
		})

	// swagger:route GET /plugin_providers/{name} PluginProviders getPluginProvider
	//
	// Get a specific plugin with {name}
	//
	// Get a specific plugin specified by {name}.
	//
	//     Produces:
	//       application/json
	//
	//     Responses:
	//       200: PluginProviderResponse
	//       401: NoContentResponse
	//       403: NoContentResponse
	//       404: ErrorResponse
	//       500: ErrorResponse
	f.ApiGroup.GET("/plugin_providers/:name",
		func(c *gin.Context) {
			if !assureAuth(c, f.Logger, "plugin_providers", "get", c.Param(`name`)) {
				return
			}

			pp := f.pc.GetPluginProvider(c.Param(`name`))
			if pp != nil {
				c.JSON(http.StatusOK, pp)
			} else {
				c.JSON(http.StatusNotFound,
					backend.NewError("API_ERROR", http.StatusNotFound,
						fmt.Sprintf("plugin provider get: not found: %s", c.Param(`name`))))
			}

		})
}
