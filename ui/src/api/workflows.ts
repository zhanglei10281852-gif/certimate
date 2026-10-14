import { WORKFLOW_TRIGGERS, type WorkflowGraph, type WorkflowModel } from "@/domain/workflow";

import { get as httpGet, post as httpPost } from "./_api";

export const getStats = () => {
  type RespData = {
    concurrency: number;
    pendingRunIds: string[];
    processingRunIds: string[];
  };

  return httpGet<RespData>({
    url: `/api/workflows/stats`,
  });
};

// 保存草稿：仅推进 baseDraftVersion 所指向的草稿代际；版本落后时服务端返回 409
export const saveWorkflowDraft = (workflowId: string, data: { graph: WorkflowGraph; baseDraftVersion: number }) => {
  return httpPost<WorkflowModel>({
    url: `/api/workflows/${encodeURIComponent(workflowId)}/draft`,
    body: {
      graph: data.graph,
      baseDraftVersion: data.baseDraftVersion,
    },
  });
};

// 发布：服务端原子校验草稿代际，并把图、触发方式、定时表达式作为同一个已发布代提交
export const publishWorkflowDraft = (workflowId: string, data: { baseDraftVersion: number }) => {
  return httpPost<WorkflowModel>({
    url: `/api/workflows/${encodeURIComponent(workflowId)}/publish`,
    body: {
      baseDraftVersion: data.baseDraftVersion,
    },
  });
};

// 回滚：用服务端当前已发布内容重置草稿，同样要求草稿代际匹配
export const rollbackWorkflowDraft = (workflowId: string, data: { baseDraftVersion: number }) => {
  return httpPost<WorkflowModel>({
    url: `/api/workflows/${encodeURIComponent(workflowId)}/rollback`,
    body: {
      baseDraftVersion: data.baseDraftVersion,
    },
  });
};

export const startRun = (workflowId: string) => {
  return httpPost({
    url: `/api/workflows/${encodeURIComponent(workflowId)}/runs`,
    body: {
      trigger: WORKFLOW_TRIGGERS.MANUAL,
    },
  });
};

export const cancelRun = (workflowId: string, runId: string) => {
  return httpPost({
    url: `/api/workflows/${encodeURIComponent(workflowId)}/runs/${encodeURIComponent(runId)}/cancel`,
  });
};
