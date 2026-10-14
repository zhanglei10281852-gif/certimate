import { ClientResponseError } from "pocketbase";

export const unwrapErrMsg = (error: unknown): string => {
  if (error instanceof ClientResponseError) {
    return Object.keys(error.response ?? {}).length ? unwrapErrMsg(error.response) : error.message;
  } else if (error instanceof Error) {
    return error.message;
  } else if (typeof error === "object" && error != null) {
    if ("message" in error) {
      return unwrapErrMsg(error.message);
    } else if ("msg" in error) {
      return unwrapErrMsg(error.msg);
    }
  } else if (typeof error === "string") {
    return error || "Unknown error";
  }

  return "Unknown error";
};

// 工作流草稿代际冲突的业务码，与后端 domain.ErrWorkflowVersionConflict / 记录更新守卫保持一致
export const WORKFLOW_VERSION_CONFLICT_CODE = 409;

// 识别"他人已更新工作流"的冲突结果：
// - 版本化接口业务错误：HTTP 200，响应体 code=409（被 _api 包装为 status=409 的 ClientResponseError）
// - 通用记录更新守卫：HTTP 409
export const isWorkflowVersionConflictError = (error: unknown): boolean => {
  if (error instanceof ClientResponseError) {
    if (error.status === WORKFLOW_VERSION_CONFLICT_CODE) return true;
    const resp = error.response as { code?: unknown; data?: { code?: string } } | undefined;
    if (resp?.code === WORKFLOW_VERSION_CONFLICT_CODE) return true;
    if (resp?.data?.code === "WORKFLOW_VERSION_CONFLICT") return true;
  }
  return false;
};
