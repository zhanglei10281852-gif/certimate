package dtos

import (
	"github.com/certimate-go/certimate/internal/domain"
)

type WorkflowStartRunReq struct {
	WorkflowId string                     `json:"-"`
	RunTrigger domain.WorkflowTriggerType `json:"trigger"`
}

type WorkflowStartRunResp struct {
	RunId string `json:"runId"`
}

type WorkflowCancelRunReq struct {
	WorkflowId string `bind:"path" json:"-"`
	RunId      string `bind:"path" json:"-"`
}

type WorkflowCancelRunResp struct{}

type WorkflowStatisticsResp struct {
	Concurrency      int      `json:"concurrency"`
	PendingRunIds    []string `json:"pendingRunIds"`
	ProcessingRunIds []string `json:"processingRunIds"`
}

// WorkflowSaveDraftReq 仅推进 BaseDraftVersion 所指向的草稿代际。
type WorkflowSaveDraftReq struct {
	WorkflowId       string                `json:"-"`
	Graph            *domain.WorkflowGraph `json:"graph"`
	BaseDraftVersion int                   `json:"baseDraftVersion"`
}

// WorkflowPublishDraftReq 要求服务端当前草稿代际仍等于 BaseDraftVersion，
// 然后把该草稿（图、触发方式、定时表达式）作为同一个已发布代原子提交。
type WorkflowPublishDraftReq struct {
	WorkflowId       string `json:"-"`
	BaseDraftVersion int    `json:"baseDraftVersion"`
}

// WorkflowRollbackDraftReq 用当前已发布内容重置草稿，要求草稿代际仍等于 BaseDraftVersion。
type WorkflowRollbackDraftReq struct {
	WorkflowId       string `json:"-"`
	BaseDraftVersion int    `json:"baseDraftVersion"`
}
