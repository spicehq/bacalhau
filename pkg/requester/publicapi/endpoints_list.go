package publicapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/bacalhau-project/bacalhau/pkg/bacerrors"
	"github.com/bacalhau-project/bacalhau/pkg/jobstore"
	"github.com/bacalhau-project/bacalhau/pkg/model"
	"github.com/bacalhau-project/bacalhau/pkg/models/migration/legacy"
	"github.com/bacalhau-project/bacalhau/pkg/publicapi/handlerwrapper"
	"github.com/rs/zerolog/log"
)

type listRequest struct {
	JobID       string   `json:"id" example:"9304c616-291f-41ad-b862-54e133c0149e"`
	ClientID    string   `json:"client_id" example:"ac13188e93c97a9c2e7cf8e86c7313156a73436036f30da1ececc2ce79f9ea51"`
	IncludeTags []string `json:"include_tags" example:"['any-tag']"`
	ExcludeTags []string `json:"exclude_tags" example:"['any-tag']"`
	MaxJobs     int      `json:"max_jobs" example:"10"`
	ReturnAll   bool     `json:"return_all" `
	SortBy      string   `json:"sort_by" example:"created_at"`
	SortReverse bool     `json:"sort_reverse"`
}

type ListRequest = listRequest

type listResponse struct {
	Jobs []*model.JobWithInfo `json:"jobs"`
}

type ListResponse = listResponse

// list godoc
//
//	@ID						pkg/requester/publicapi/list
//	@Summary				Simply lists jobs.
//	@Description.markdown	endpoints_list
//	@Tags					Job
//	@Accept					json
//	@Produce				json
//	@Param					listRequest	body		listRequest	true	"Set `return_all` to `true` to return all jobs on the network (may degrade performance, use with care!)."
//	@Success				200			{object}	listResponse
//	@Failure				400			{object}	string
//	@Failure				500			{object}	string
//	@Router					/requester/list [post]
//
//nolint:lll
func (s *RequesterAPIServer) list(res http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var listReq ListRequest
	if err := json.NewDecoder(req.Body).Decode(&listReq); err != nil {
		http.Error(res, err.Error(), http.StatusBadRequest)
		return
	}
	res.Header().Set(handlerwrapper.HTTPHeaderClientID, listReq.ClientID)
	res.Header().Set(handlerwrapper.HTTPHeaderJobID, listReq.JobID)

	jobList, err := s.getJobsList(ctx, listReq)
	if err != nil {
		_, ok := err.(*bacerrors.JobNotFound)
		if ok {
			http.Error(res, bacerrors.ErrorToErrorResponse(err), http.StatusBadRequest)
			return
		}
	}

	jobWithInfos := make([]*model.JobWithInfo, len(jobList))
	for i, job := range jobList {
		jobState, innerErr := legacy.GetJobState(ctx, s.jobStore, job.ID())
		if innerErr != nil {
			log.Ctx(ctx).Error().Err(innerErr).Msg("error getting job states")
			http.Error(res, err.Error(), http.StatusInternalServerError)
			return
		}
		jobWithInfos[i] = &model.JobWithInfo{
			Job:   job,
			State: jobState,
		}
	}
	res.WriteHeader(http.StatusOK)
	err = json.NewEncoder(res).Encode(ListResponse{
		Jobs: jobWithInfos,
	})
	if err != nil {
		http.Error(res, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *RequesterAPIServer) getJobsList(ctx context.Context, listReq ListRequest) ([]model.Job, error) {
	list, err := s.jobStore.GetJobs(ctx, jobstore.JobQuery{
		Namespace:   listReq.ClientID,
		ID:          listReq.JobID,
		Limit:       listReq.MaxJobs,
		IncludeTags: listReq.IncludeTags,
		ExcludeTags: listReq.ExcludeTags,
		ReturnAll:   listReq.ReturnAll,
		SortBy:      listReq.SortBy,
		SortReverse: listReq.SortReverse,
	})
	if err != nil {
		return nil, err
	}
	res := make([]model.Job, len(list))
	for i := range list {
		legacyJob, err := legacy.ToLegacyJob(&list[i])
		if err != nil {
			return nil, err
		}
		res[i] = *legacyJob
	}
	return res, nil
}
