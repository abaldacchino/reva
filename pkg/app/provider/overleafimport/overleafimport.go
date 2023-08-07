// Copyright 2018-2023 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package overleafimport

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	appprovider "github.com/cs3org/go-cs3apis/cs3/app/provider/v1beta1"
	appregistry "github.com/cs3org/go-cs3apis/cs3/app/registry/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/pkg/app"
	"github.com/cs3org/reva/pkg/app/provider/registry"
	"github.com/cs3org/reva/pkg/appctx"
	"github.com/cs3org/reva/pkg/rgrpc/todo/pool"
	"github.com/cs3org/reva/pkg/rhttp"
	"github.com/cs3org/reva/pkg/sharedconf"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type overleafImportProvider struct {
	conf           *config
	overleafClient *http.Client
}

func (p *overleafImportProvider) GetAppURL(ctx context.Context, resource *provider.ResourceInfo, viewMode appprovider.ViewMode, token string, opaqueMap map[string]*typespb.OpaqueEntry, language string) (*appprovider.OpenInAppURL, error) {
	log := appctx.GetLogger(ctx)

	client, err := pool.GetGatewayServiceClient(pool.Endpoint(sharedconf.GetGatewaySVC("")))
	if err != nil {
		return nil, errors.Wrap(err, "overleaf: error fetching gateway service client.")
	}

	// Resource must be already Imported to be imported again
	statRes, err := client.Stat(ctx, &provider.StatRequest{
		Ref: &provider.Reference{
			ResourceId: resource.Id,
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "overleaf: error statting file.")
	}

	_, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.time"]
	if !found {
		return nil, errors.New("overleaf import: file not previously exported, error getting file export time")
	}
	name, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.name"]
	if !found {
		return nil, errors.New("overleaf import: error getting file export name")
	}

	projectid, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.projectid"]

	// If not found we try to resolve using export time and project name
	if !found {
		projectUrl, err := url.Parse(p.conf.AppURL)

		if err != nil {
			return nil, errors.Wrap(err, "overleaf import: error parsing app provider url")
		}

		projectUrl.Path = path.Join(projectUrl.Path, "/project")

		httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, projectUrl.String(), nil)
		if err != nil {
			return nil, err
		}

		log.Debug().Str("get projects url", httpReq.URL.String()).Msg("Sending project request to Overleaf server")
		getProjectsRes, err := p.overleafClient.Do(httpReq)
		if err != nil {
			return nil, errors.Wrap(err, "overleaf import: error performing project request to Overleaf server")
		}
		defer getProjectsRes.Body.Close()

		body, err := io.ReadAll(getProjectsRes.Body)
		if err != nil {
			return nil, err
		}

		sbody := string(body)
		// Restrict text to contain the information of our project onwards
		restrictedText := sbody[strings.Index(sbody, "&quot;"+name+"&quot;"):]
		// Restrict text to start at project id
		restrictedText = restrictedText[strings.Index(restrictedText, "&quot;id&quot;:&quot;")+len("&quot;id&quot;:&quot;"):]
		// Finding end of project id
		projectid = restrictedText[:strings.Index(restrictedText, "&quot")]
	}

	downloadUrl, err := url.Parse(p.conf.AppURL)

	if err != nil {
		return nil, errors.Wrap(err, "overleaf import: error parsing app provider url")
	}

	downloadUrl.Path = path.Join(downloadUrl.Path, "/project/", projectid, "/download/zip")

	httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, downloadUrl.String(), nil)
	if err != nil {
		return nil, err
	}

	q := httpReq.URL.Query()
	httpReq.URL.RawQuery = q.Encode()

	httpReq.Header.Set("Cookie", p.conf.Cookie)
	httpReq.Header.Set("Host", "www.overleaf.com")

	log.Debug().Str("url", httpReq.URL.String()).Msg("Sending request to Overleaf server")
	downloadRes, err := p.overleafClient.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, "overleaf import: error performing open request to Overleaf server")
	}
	defer downloadRes.Body.Close()

	_, err = io.ReadAll(downloadRes.Body)
	if err != nil {
		return nil, err
	}
	if downloadRes.StatusCode != http.StatusOK {
		// Overleaf server returned failure
		return nil, errors.New("overleaf import: failed to make request to Overleaf server")
	}

	return &appprovider.OpenInAppURL{
		AppUrl: "https://www.overleaf.com",
		Method: http.MethodGet,
		Target: appprovider.Target_TARGET_BLANK,
	}, nil
}

func (p *overleafImportProvider) GetAppProviderInfo(ctx context.Context) (*appregistry.ProviderInfo, error) {
	return &appregistry.ProviderInfo{
		Name:      "Overleaf",
		MimeTypes: p.conf.MimeTypes,
		Icon:      p.conf.AppIconURI,
		Action:    "Import from",
	}, nil
}

func init() {
	registry.Register("overleafimport", New)
}

type config struct {
	MimeTypes           []string `mapstructure:"mime_types" docs:"nil;Inherited from the appprovider."`
	AppName             string   `mapstructure:"app_name" docs:";The App user-friendly name."`
	AppIconURI          string   `mapstructure:"app_icon_uri" docs:";A URI to a static asset which represents the app icon."`
	AppURL              string   `mapstructure:"app_url" docs:";The App URL."`
	AppIntURL           string   `mapstructure:"app_int_url" docs:";The internal app URL in case of dockerized deployments. Defaults to AppURL"`
	InsecureConnections bool     `mapstructure:"insecure_connections"`
	Cookie              string   `mapstructure:"cookie" docs:"Stores user cookie to access files as a temporary workaround"`
}

func parseConfig(m map[string]interface{}) (*config, error) {
	c := &config{}
	if err := mapstructure.Decode(m, c); err != nil {
		return nil, err
	}
	return c, nil
}

// New returns an implementation to of the app.Provider interface that
// connects to an application in the backend.
func New(ctx context.Context, m map[string]interface{}) (app.Provider, error) {
	c, err := parseConfig(m)
	if err != nil {
		return nil, err
	}

	// Need http client to make requests to Overleaf server
	overleafClient := rhttp.GetHTTPClient(
		rhttp.Timeout(time.Duration(10*int64(time.Second))),
		rhttp.Insecure(c.InsecureConnections),
	)
	overleafClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &overleafImportProvider{conf: c, overleafClient: overleafClient}, nil
}
