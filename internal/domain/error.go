package domain

var (
	ErrInvalidParams  = NewError(400, "invalid params")
	ErrRecordNotFound = NewError(404, "record not found")
	// ErrWorkflowVersionConflict 表示客户端所基于的草稿代际已落后于服务端持久版本。
	// 冲突时不做静默合并或重试覆盖，由客户端提示后重新加载。
	ErrWorkflowVersionConflict = NewError(409, "workflow version conflict: the workflow has been modified by another user, please reload and try again")
	// ErrWorkflowNotPublished 表示工作流尚不存在已发布快照（无法回滚或发布空草稿等）。
	ErrWorkflowNotPublished = NewError(400, "workflow has no published content")
)

type Error struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func NewError(code int, msg string) *Error {
	if code == 0 {
		code = -1
	}

	return &Error{code, msg}
}

func (e *Error) Error() string {
	return e.Msg
}

func IsRecordNotFoundError(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.Code == ErrRecordNotFound.Code
	}
	return false
}
