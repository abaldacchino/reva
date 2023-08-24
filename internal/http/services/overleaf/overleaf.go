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

package overleaf

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	storagepb "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/internal/http/services/datagateway"
	"github.com/cs3org/reva/internal/http/services/reqres"
	"github.com/cs3org/reva/pkg/appctx"
	ctxpkg "github.com/cs3org/reva/pkg/ctx"
	"github.com/cs3org/reva/pkg/rgrpc/todo/pool"
	"github.com/cs3org/reva/pkg/rhttp"
	"github.com/cs3org/reva/pkg/rhttp/global"
	"github.com/cs3org/reva/pkg/sharedconf"
	"github.com/cs3org/reva/pkg/storage/utils/walker"
	"github.com/cs3org/reva/pkg/utils/cfg"
	"github.com/cs3org/reva/pkg/utils/resourceid"
	"github.com/go-chi/chi/v5"
	"github.com/pkg/errors"
)

type svc struct {
	conf       *config
	gtwClient  gateway.GatewayAPIClient
	httpClient *http.Client
	router     *chi.Mux
}

type config struct {
	Prefix      string `mapstructure:"prefix"`
	GatewaySvc  string `mapstructure:"gatewaysvc"  validate:"required"`
	AppName     string `mapstructure:"app_name" docs:";The App user-friendly name."  validate:"required"`
	ArchiverURL string `mapstructure:"archiver_url" docs:";Internet-facing URL of the archiver service, used to serve the files to Overleaf."  validate:"required"`
	AppURL      string `mapstructure:"app_url" docs:";The App URL."   validate:"required"`
	Insecure    bool   `mapstructure:"insecure" docs:"false;Whether to skip certificate checks when sending requests."`
	Cookie      string `mapstructure:"cookie" docs:"Stores user cookie to access files as a temporary workaround"`
}

func init() {
	global.Register("overleaf", New)
}

func New(ctx context.Context, m map[string]interface{}) (global.Service, error) {
	var conf config
	if err := cfg.Decode(m, &conf); err != nil {
		return nil, err
	}

	gtw, err := pool.GetGatewayServiceClient(pool.Endpoint(conf.GatewaySvc))
	if err != nil {
		return nil, err
	}

	// Need http client to make requests to Overleaf server
	httpClient := rhttp.GetHTTPClient(
		rhttp.Timeout(time.Duration(10*int64(time.Second))),
		rhttp.Insecure(conf.Insecure),
	)
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	r := chi.NewRouter()

	s := &svc{
		conf:       &conf,
		gtwClient:  gtw,
		httpClient: httpClient,
		router:     r,
	}

	if err := s.routerInit(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *svc) routerInit() error {
	s.router.Get("/import", s.handleImport)
	s.router.Post("/export", s.handleExport)
	return nil
}

func (c *config) ApplyDefaults() {
	if c.Prefix == "" {
		c.Prefix = "overleaf"
	}

	c.GatewaySvc = sharedconf.GetGatewaySVC(c.GatewaySvc)
}

// Close performs cleanup.
func (s *svc) Close() error {
	return nil
}

func (s *svc) Prefix() string {
	return s.conf.Prefix
}

func (s *svc) Unprotected() []string {
	return nil
}

func (s *svc) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.router.ServeHTTP(w, r)
	})
}

func (s *svc) handleImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := appctx.GetLogger(ctx)

	statRes, resourceRef, err := s.validateQuery(w, r, ctx)
	if err != nil {
		// Validate query handles errors
		return
	}

	resource := statRes.Info

	// User needs to have edit rights to import from Overleaf
	if !resource.PermissionSet.InitiateFileUpload || !resource.PermissionSet.InitiateFileDownload {
		reqres.WriteError(w, r, reqres.APIErrorUnauthenticated, "permission denied when accessing the file", err)
		return
	}

	if resource.Type != storagepb.ResourceType_RESOURCE_TYPE_FILE && resource.Type != storagepb.ResourceType_RESOURCE_TYPE_CONTAINER {
		reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "invalid resource type, resource should be a file or a folder", nil)
		return
	}

	token, ok := ctxpkg.ContextGetToken(ctx)
	if !ok || token == "" {
		reqres.WriteError(w, r, reqres.APIErrorUnauthenticated, "Access token is invalid or empty", err)
		return
	}

	projectId, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.projectid"]

	// If not found we try to resolve using export time and project name
	// Once Overleaf access is given, this can becomes redundant as we would be able to find the project id during export
	if !found {
		// Get name and export time in order to match with existing projects
		exportTimeStr, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.exporttime"]
		if !found {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "overleaf import: file not previously exported, error getting file export time", nil)
			return
		}
		exportTime, err := strconv.Atoi(exportTimeStr)
		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "overleaf import: exportTime not in the correct format", nil)
			return
		}

		encodedName, found := statRes.Info.GetArbitraryMetadata().Metadata["reva.overleaf.name"]
		if !found {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "overleaf import: error getting file export name", nil)
			return
		}
		decodedName, err := base64.StdEncoding.DecodeString(encodedName)
		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "overleaf import: error decoding project name", nil)
			return
		}
		name := string(decodedName)

		// Making request to get project list
		projectUrl, err := url.Parse(s.conf.AppURL)

		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error parsing app provider url", err)
			return
		}

		projectUrl.Path = path.Join(projectUrl.Path, "/project")

		httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, projectUrl.String(), nil)
		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: making http request", err)
			return
		}
		httpReq.Header.Set("Cookie", s.conf.Cookie)
		httpReq.Header.Set("Host", "www.overleaf.com")
		log.Debug().Str("cookie", s.conf.Cookie).Msg("Overleaf cookie being sent")

		log.Debug().Str("get projects url", httpReq.URL.String()).Msg("Sending project request to Overleaf server")
		getProjectsRes, err := s.httpClient.Do(httpReq)
		if err != nil {
			log.Err(err).Msg("overleaf import: error performing project request to Overleaf server")
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error performing project request to Overleaf server", err)
			return
		}
		defer getProjectsRes.Body.Close()

		if getProjectsRes.StatusCode != http.StatusOK {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: Overleaf server returned an error", err)
			return
		}

		body, err := io.ReadAll(getProjectsRes.Body)
		if err != nil {
			log.Err(err).Msg("overleaf import: error reading body")
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error reading body", err)
			return
		}
		sbody := string(body)

		// Name might be in form <name> (<number>) after being exported to Overleaf, due to name conflicts
		expr, err := regexp.Compile("&quot;" + regexp.QuoteMeta(name) + "( \\(([0-9]+)\\))?" + "&quot;")

		if err != nil {
			log.Err(err).Msg("overleaf import: error compiling regex, project name decoding might be faulty.")
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error compiling regex, project name decoding might be faulty.", err)
			return
		}

		indices := expr.FindAllStringIndex(sbody, -1)

		if indices == nil {
			reqres.WriteError(w, r, reqres.APIErrorNotFound, "overleaf import: no matching project found", err)
			return
		}

		// Distance is the time between export in CERNBox and project creation in Overleaf
		// To ensure a project match, we enforce that project creation can happen up to 30 seconds after export in CERNBox.
		distance := 30001

		for _, index := range indices {
			restrictedText := sbody[:index[0]]
			// Restrict text to start at project id value beginning
			restrictedText = restrictedText[strings.LastIndex(restrictedText, "&quot;id&quot;:&quot;")+len("&quot;id&quot;:&quot;"):]
			newProjectId := restrictedText[:strings.Index(restrictedText, "&quot")]

			projectExportTime, err := s.getProjectCreationTime(ctx, newProjectId)
			if err != nil {
				reqres.WriteError(w, r, reqres.APIErrorNotFound, "overleaf import: unable to find project creation time", err)
				return
			}

			// Picking project closest to export time
			newDistance := (projectExportTime / 1000) - exportTime
			if newDistance >= 0 && // Project exported at or after export time
				newDistance < distance {
				distance = newDistance
				projectId = newProjectId
			}
		}

		// If distance unchanged, no matching project was found
		if distance == 30001 {
			reqres.WriteError(w, r, reqres.APIErrorNotFound, "overleaf import: no matching project found based on export time", err)
			return
		}

		// Otherwise store project id for sync later
		req := &provider.SetArbitraryMetadataRequest{
			Ref: &provider.Reference{
				ResourceId: resource.Id,
			},
			ArbitraryMetadata: &provider.ArbitraryMetadata{
				Metadata: map[string]string{
					"reva.overleaf.projectid": projectId,
				},
			},
		}

		res, err := s.gtwClient.SetArbitraryMetadata(ctx, req)

		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error setting arbitrary metadata", nil)
			return
		}
		if res.Status.Code != rpc.Code_CODE_OK {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error statting", nil)
			return
		}
	}

	downloadUrl, err := url.Parse(s.conf.AppURL)

	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error parsing app provider url", err)
		return
	}

	downloadUrl.Path = path.Join(downloadUrl.Path, "/project/", projectId, "/download/zip")

	httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, downloadUrl.String(), nil)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error creating http request", err)
		return
	}

	httpReq.Header.Set("Cookie", s.conf.Cookie)
	httpReq.Header.Set("Host", "www.overleaf.com")

	log.Debug().Str("url", httpReq.URL.String()).Msg("Sending request to Overleaf server")
	downloadRes, err := s.httpClient.Do(httpReq)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error performing open request to Overleaf server", err)
		return
	}
	defer downloadRes.Body.Close()

	zipFile, err := io.ReadAll(downloadRes.Body)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: error reading Overleaf response", err)
		return
	}
	if downloadRes.StatusCode != http.StatusOK {
		// Overleaf server returned failure
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf import: failed to make request to Overleaf server", err)
		return
	}

	// 50 MB limit
	if len(zipFile) > 1024*1024*50 {
		reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "Project is too large to import", nil)
		return
	}

	zipReader, err := zip.NewReader(bytes.NewReader(zipFile), int64(len(zipFile)))
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "unable to create zip reader:"+err.Error(), err)
		return
	}

	// Handle case when resource is a file
	if resource.Type == provider.ResourceType_RESOURCE_TYPE_FILE && r.Form.Get("force_import") == "" {
		if len(zipReader.File) == 1 {
			unzippedFile, err := zipReader.File[0].Open()
			// Converting to []byte instead of using Reader immediately because we need to know size
			if err != nil {
				reqres.WriteError(w, r, reqres.APIErrorServerError, "unable to unzip file", err)
				return
			}
			data, err := io.ReadAll(unzippedFile)
			unzippedFile.Close()

			// Overwriting current resource
			fileRef := &storagepb.Reference{
				Path: resourceRef.Path,
			}

			err = s.uploadFile(data, fileRef, w, r, ctx)
			if err != nil {
				return
			}
		} else {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "Multiple files detected in project when importing into a single file", nil)
			return
		}
	} else { // Case when we want to upload all contents of project
		basePath := ""
		if resource.Type == provider.ResourceType_RESOURCE_TYPE_FILE {
			basePath = filepath.Dir(resourceRef.Path)
		} else {
			basePath = resourceRef.Path
		}

		for _, readFile := range zipReader.File {
			// readFile is never a directory, but check is done just in case .File method changes
			if !readFile.FileInfo().IsDir() {
				unzippedFile, err := readFile.Open()
				// Converting to []byte instead of using Reader immediately because we need to know size
				if err != nil {
					reqres.WriteError(w, r, reqres.APIErrorServerError, "unable to unzip file", err)
					return
				}
				data, err := io.ReadAll(unzippedFile)
				unzippedFile.Close()

				fileRef := &storagepb.Reference{
					Path: path.Join(basePath, readFile.Name),
				}

				err = s.uploadFile(data, fileRef, w, r, ctx)
				if err != nil {
					return
				}
			}
		}
	}

	// Deleting extra contents only done in case that resource is a folder
	if resource.Type == provider.ResourceType_RESOURCE_TYPE_CONTAINER {
		walker := walker.NewWalker(s.gtwClient)
		err = walker.Walk(ctx, resourceRef.Path, func(path string, info *provider.ResourceInfo, err error) error {
			log.Debug().Msg("Walking to " + info.Path + "...")
			// Traverse every file/folder except for root
			if info.Path != resourceRef.Path {
				_, err := zipReader.Open(info.Path[len(resourceRef.Path)+1:])
				// defer reader.Close()		// Closing zip Reader breaks walk code
				if err != nil {
					log.Debug().Msg("Deleting " + info.Path + "...")
					ref := &storagepb.Reference{
						Path: info.Path,
					}
					req := &provider.DeleteRequest{Ref: ref}
					res, err := s.gtwClient.Delete(ctx, req)

					if err != nil {
						return err
					} else if res.Status.Code != rpc.Code_CODE_OK && res.Status.Code != rpc.Code_CODE_NOT_FOUND {
						// Not found is not an error as we might've deleted the resource's parent folder
						return errors.New("Error deleting file")
					}
				}
			}
			return nil
		})

		if err != nil {
			reqres.WriteError(w, r, reqres.APIErrorServerError, "Unable to sync deleted files", err)
		}
	}

	// No need to return anything, so content is empty here.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func (s *svc) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := appctx.GetLogger(ctx)

	statRes, _, err := s.validateQuery(w, r, ctx)
	if err != nil {
		// Validate query handles errors
		return
	}

	resource := statRes.Info

	// User needs to have download rights to export to Overleaf
	if !resource.PermissionSet.InitiateFileDownload {
		reqres.WriteError(w, r, reqres.APIErrorUnauthenticated, "permission denied when accessing the file", err)
		return
	}

	if resource.Type != storagepb.ResourceType_RESOURCE_TYPE_FILE && resource.Type != storagepb.ResourceType_RESOURCE_TYPE_CONTAINER {
		reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "invalid resource type, resource should be a file or a folder", nil)
		return
	}

	token, ok := ctxpkg.ContextGetToken(ctx)
	if !ok || token == "" {
		reqres.WriteError(w, r, reqres.APIErrorUnauthenticated, "Access token is invalid or empty", err)
		return
	}

	if r.Form.Get("override") == "" {
		creationTime, alreadySet := resource.GetArbitraryMetadata().Metadata["reva.overleaf.exporttime"]
		if alreadySet {
			w.WriteHeader(http.StatusConflict)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"code":        "ALREADY_EXISTS",
				"message":     "Project was already exported",
				"export_time": creationTime,
			}); err != nil {
				reqres.WriteError(w, r, reqres.APIErrorServerError, "error marshalling JSON response", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			return
		}
	}

	// TODO: generate and use a more restricted token
	restrictedToken := token

	// Setting up archiver request
	archHTTPReq, err := rhttp.NewRequest(ctx, http.MethodGet, s.conf.ArchiverURL, nil)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf: error setting up http request", nil)
		return
	}

	archQuery := archHTTPReq.URL.Query()
	archQuery.Add("id", resource.Id.StorageId+"!"+resource.Id.OpaqueId)
	archQuery.Add("access_token", restrictedToken)
	archQuery.Add("arch_type", "zip")

	archHTTPReq.URL.RawQuery = archQuery.Encode()
	log.Debug().Str("Archiver url", archHTTPReq.URL.String()).Msg("URL for downloading zipped resource from archiver")

	// Setting up Overleaf request
	appUrl := s.conf.AppURL + "/docs"
	httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, appUrl, nil)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf: error setting up http request", nil)
		return
	}

	q := httpReq.URL.Query()

	// snip_uri is link to archiver request
	q.Add("snip_uri", archHTTPReq.URL.String())

	// getting file/folder name so as not to expose authentication token in project name
	name := strings.TrimSuffix(filepath.Base(resource.Path), filepath.Ext(resource.Path))
	q.Add("snip_name", name)

	httpReq.URL.RawQuery = q.Encode()
	url := httpReq.URL.String()

	req := &provider.SetArbitraryMetadataRequest{
		Ref: &provider.Reference{
			ResourceId: resource.Id,
		},
		ArbitraryMetadata: &provider.ArbitraryMetadata{
			Metadata: map[string]string{
				"reva.overleaf.exporttime": strconv.Itoa(int(time.Now().Unix())),
				"reva.overleaf.name":       base64.StdEncoding.EncodeToString([]byte(name)),
			},
		},
	}

	res, err := s.gtwClient.SetArbitraryMetadata(ctx, req)

	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf: error setting arbitrary metadata", nil)
		return
	}

	if res.Status.Code != rpc.Code_CODE_OK {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "overleaf: error statting", nil)
		return
	}

	if err := json.NewEncoder(w).Encode(map[string]any{
		"app_url": url,
	}); err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "error marshalling JSON response", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

func (s *svc) getProjectCreationTime(ctx context.Context, projectId string) (int, error) {
	log := appctx.GetLogger(ctx)

	nextBeforeTimestamp := 0

	// Need to loop since Overleaf does not expose all updates at a time
	for {
		getUpdatesUrl, err := url.Parse(s.conf.AppURL)

		if err != nil {
			return -1, errors.Wrap(err, "overleaf import: error parsing app provider url")
		}

		getUpdatesUrl.Path = path.Join(getUpdatesUrl.Path, "/project/", projectId, "/updates")

		httpReq, err := rhttp.NewRequest(ctx, http.MethodGet, getUpdatesUrl.String(), nil)
		if err != nil {
			return -1, errors.Wrap(err, "overleaf import: unable to create new http request")
		}

		q := httpReq.URL.Query()
		q.Add("min_count", "10")
		if nextBeforeTimestamp != 0 {
			q.Add("before", strconv.Itoa(nextBeforeTimestamp))
		}

		httpReq.URL.RawQuery = q.Encode()

		httpReq.Header.Set("Cookie", s.conf.Cookie)
		httpReq.Header.Set("Host", "www.overleaf.com")

		getUpdatesRes, err := s.httpClient.Do(httpReq)
		if err != nil {
			log.Err(err).Msg("overleaf import: error performing project request to Overleaf server")
			return -1, errors.Wrap(err, "overleaf import: error performing project request to Overleaf server")
		}
		defer getUpdatesRes.Body.Close()

		body, err := io.ReadAll(getUpdatesRes.Body)

		var result map[string]interface{}
		err = json.Unmarshal(body, &result)
		if err != nil {
			return -1, err
		}

		nextBeforeTimestampInterface, remainingUpdates := result["nextBeforeTimeStamp"]
		if remainingUpdates {
			nextBeforeTimestamp = nextBeforeTimestampInterface.(int)
		} else {
			updatesInterface, ok := result["updates"]
			if !ok {
				return -1, errors.Wrap(err, "overleaf import: unable to get updates field from Overleaf response")
			}

			updates := updatesInterface.([]interface{})
			lastUpdateInterface := updates[len(updates)-1]
			lastUpdate := lastUpdateInterface.(map[string]interface{})
			metaData := lastUpdate["meta"].(map[string]interface{})
			time := int(metaData["start_ts"].(float64))
			return time, nil
		}
	}
}

func (s *svc) validateQuery(w http.ResponseWriter, r *http.Request, ctx context.Context) (*storagepb.StatResponse, *storagepb.Reference, error) {
	if err := r.ParseForm(); err != nil {
		reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "parameters could not be parsed", nil)
		return nil, nil, err
	}

	resourceID := r.Form.Get("resource_id")

	var resourceRef storagepb.Reference
	if resourceID == "" {
		path := r.Form.Get("path")
		if path == "" {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "missing resource ID or path", nil)
			return nil, nil, errors.New("missing resource ID or path")
		}
		resourceRef.Path = path
	} else {
		resourceID := resourceid.OwnCloudResourceIDUnwrap(resourceID)
		if resourceID == nil {
			reqres.WriteError(w, r, reqres.APIErrorInvalidParameter, "invalid resource ID", nil)
			return nil, nil, errors.New("invalid resource ID")
		}
		resourceRef.ResourceId = resourceID
	}

	statRes, err := s.gtwClient.Stat(ctx, &storagepb.StatRequest{Ref: &resourceRef})
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "Internal error accessing the resource, please try again later", err)
		return nil, nil, errors.New("Internal error accessing the resource, please try again later")
	}

	if statRes.Status.Code == rpc.Code_CODE_NOT_FOUND {
		reqres.WriteError(w, r, reqres.APIErrorNotFound, "resource does not exist", nil)
		return nil, nil, errors.New("resource does not exist")
	} else if statRes.Status.Code != rpc.Code_CODE_OK {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "failed to stat the resource", nil)
		return nil, nil, errors.New("failed to stat the resource")
	}

	return statRes, &resourceRef, nil
}

func (s *svc) uploadFile(data []byte, fileRef *storagepb.Reference, w http.ResponseWriter, r *http.Request, ctx context.Context) error {
	log := appctx.GetLogger(ctx)

	createReq := &storagepb.InitiateFileUploadRequest{
		Ref: fileRef,
		Opaque: &typespb.Opaque{
			Map: map[string]*typespb.OpaqueEntry{
				"Upload-Length": {
					Decoder: "plain",
					Value:   []byte(strconv.Itoa(len(data))),
				},
			},
		},
	}

	createRes, err := s.gtwClient.InitiateFileUpload(ctx, createReq)
	if err != nil {
		log.Err(err).Msg("error calling InitiateFileUpload")
		reqres.WriteError(w, r, reqres.APIErrorServerError, "error calling InitiateFileUpload", err)
		return err
	}
	if createRes.Status.Code != rpc.Code_CODE_OK {
		log.Err(err).Msg("error calling InitiateFileUpload: " + createRes.Status.Message)
		reqres.WriteError(w, r, reqres.APIErrorServerError, "error calling InitiateFileUpload", nil)
		return errors.New("error calling InitiateFileUpload")
	}

	// Do a HTTP PUT with an empty body
	var ep, uploadToken string
	for _, p := range createRes.Protocols {
		if p.Protocol == "simple" {
			ep, uploadToken = p.UploadEndpoint, p.Token
		}
	}
	httpReq, err := rhttp.NewRequest(ctx, http.MethodPut, ep, bytes.NewReader(data))
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "failed to create the file", err)
		return err
	}

	httpReq.Header.Set(datagateway.TokenTransportHeader, uploadToken)
	httpRes, err := rhttp.GetHTTPClient(
		rhttp.Context(ctx),
		rhttp.Insecure(s.conf.Insecure),
	).Do(httpReq)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "failed to create the file", err)
		return err
	}
	defer httpRes.Body.Close()
	if httpRes.StatusCode != http.StatusOK {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "failed to create the file", nil)
		return errors.New("failed to create the file")
	}

	statFileReq := &storagepb.StatRequest{
		Ref: fileRef,
	}

	// Stat the newly created file
	newStatRes, err := s.gtwClient.Stat(ctx, statFileReq)
	if err != nil {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "statting the created file failed", err)
		return err
	}

	if newStatRes.Status.Code != rpc.Code_CODE_OK {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "statting the created file failed", nil)
		return errors.New("statting the created file failed")
	}

	if newStatRes.Info.Type != storagepb.ResourceType_RESOURCE_TYPE_FILE {
		reqres.WriteError(w, r, reqres.APIErrorServerError, "the created resource is not a file", nil)
		return errors.New("the created resource is not a file")
	}
	return nil
}
