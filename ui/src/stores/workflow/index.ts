import { produce } from "immer";
import { create } from "zustand";

import { publishWorkflowDraft, rollbackWorkflowDraft, saveWorkflowDraft } from "@/api/workflows";
import { WORKFLOW_NODE_TYPES, type WorkflowModel } from "@/domain/workflow";
import { get as getWorkflow, save as saveWorkflow, subscribe as subscribeWorkflow } from "@/repository/workflow";

import { type WorkflowStore } from "./types";

// 实时事件只允许同步这些与编排编辑无关的元数据字段，
// 避免他人的保存/发布静默替换当前编辑会话所基于的草稿代际。

export const useWorkflowStore = create<WorkflowStore>((set, get) => {
  const ensureInitialized = () => {
    if (!get().initialized) throw "Workflow not initialized yet";
  };

  let unsubscriber: (() => void) | undefined;

  return {
    workflow: {} as WorkflowModel,
    initialized: false,

    init: async (id: string) => {
      const data = await getWorkflow(id);
      set({
        workflow: data,
        initialized: true,
      });

      unsubscriber ??= await subscribeWorkflow(id, (cb) => {
        if (cb.record.id !== get().workflow.id) return;

        set((state) => ({
          workflow: produce(state.workflow, (draft) => {
            // 编排相关字段（graphDraft/graphContent/hasDraft/hasContent/trigger/版本号等）不随实时事件覆盖，
            // 避免他人的变更静默并入当前编辑会话；版本落后时由服务端在下一次写入时返回冲突。
            draft.name = cb.record.name;
            draft.description = cb.record.description;
            draft.enabled = cb.record.enabled;
            draft.lastRunRef = cb.record.lastRunRef;
            draft.lastRunStatus = cb.record.lastRunStatus;
            draft.lastRunTime = cb.record.lastRunTime;
            draft.expand = cb.record.expand;
            draft.updated = cb.record.updated;
          }),
        }));
      });
    },

    destroy: () => {
      unsubscriber?.();
      unsubscriber = void 0;

      set({
        workflow: {} as WorkflowModel,
        initialized: false,
      });
    },

    setName: async (name) => {
      ensureInitialized();

      const resp = await saveWorkflow({
        id: get().workflow.id!,
        name: name || "",
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.name = resp.name;
          }),
        };
      });
    },

    setDescription: async (description) => {
      ensureInitialized();

      const resp = await saveWorkflow({
        id: get().workflow.id!,
        description: description || "",
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.description = resp.description;
          }),
        };
      });
    },

    setEnabled: async (enabled) => {
      ensureInitialized();

      const resp = await saveWorkflow({
        id: get().workflow.id!,
        enabled: enabled,
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.enabled = resp.enabled;
          }),
        };
      });
    },

    orchestrate: async (graph) => {
      ensureInitialized();

      // 只能推进当前编辑会话实际读取到的草稿代际；冲突时抛出错误，不静默合并或重试
      const resp = await saveWorkflowDraft(get().workflow.id!, {
        graph,
        baseDraftVersion: get().workflow.draftVersion ?? 0,
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.graphDraft = resp.data.graphDraft;
            draft.hasDraft = resp.data.hasDraft;
            draft.draftVersion = resp.data.draftVersion;
          }),
        };
      });
    },

    publish: async () => {
      ensureInitialized();

      const graph = get().workflow.graphDraft!;
      if (graph?.nodes?.[0]?.type !== WORKFLOW_NODE_TYPES.START) throw "Workflow nodes tree of draft in invalid";

      const resp = await publishWorkflowDraft(get().workflow.id!, {
        baseDraftVersion: get().workflow.draftVersion ?? 0,
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.trigger = resp.data.trigger;
            draft.triggerCron = resp.data.triggerCron;
            draft.graphDraft = resp.data.graphDraft;
            draft.graphContent = resp.data.graphContent;
            draft.hasContent = resp.data.hasContent;
            draft.hasDraft = resp.data.hasDraft;
            draft.draftVersion = resp.data.draftVersion;
            draft.contentVersion = resp.data.contentVersion;
          }),
        };
      });
    },

    rollback: async () => {
      ensureInitialized();

      if (get().workflow.graphContent?.nodes?.[0]?.type !== WORKFLOW_NODE_TYPES.START) throw "Workflow nodes tree of content in invalid";

      const resp = await rollbackWorkflowDraft(get().workflow.id!, {
        baseDraftVersion: get().workflow.draftVersion ?? 0,
      });

      set((state) => {
        return {
          workflow: produce(state.workflow, (draft) => {
            draft.trigger = resp.data.trigger;
            draft.triggerCron = resp.data.triggerCron;
            draft.graphDraft = resp.data.graphDraft;
            draft.hasDraft = resp.data.hasDraft;
            draft.draftVersion = resp.data.draftVersion;
          }),
        };
      });
    },
  };
});
