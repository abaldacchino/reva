package ocdavsvc

import (
	"net/http"

	rpcpb "github.com/cernbox/go-cs3apis/cs3/rpc"
	storageproviderv0alphapb "github.com/cernbox/go-cs3apis/cs3/storageprovider/v0alpha"
)

func (s *svc) doDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fn := r.URL.Path

	client, err := s.getClient()
	if err != nil {
		logger.Error(ctx, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	ref := &storageproviderv0alphapb.Reference{
		Spec: &storageproviderv0alphapb.Reference_Path{Path: fn},
	}
	req := &storageproviderv0alphapb.DeleteRequest{Ref: ref}
	res, err := client.Delete(ctx, req)
	if err != nil {
		logger.Error(ctx, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if res.Status.Code == rpcpb.Code_CODE_NOT_FOUND {
		logger.Println(ctx, res.Status)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if res.Status.Code != rpcpb.Code_CODE_OK {
		logger.Println(ctx, res.Status)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
