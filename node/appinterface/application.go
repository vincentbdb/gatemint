package appinterface

type Application interface {
	Query(QueryParam) ResponseQuery // Query for state

	CheckTx(RequestCheckTx) ResponseCheckTx
}

const (
	CodeTypeOK uint32 = 0
)

const (
	CheckTxType_New     CheckTxType = 0
	CheckTxType_Recheck CheckTxType = 1
)

type QueryParam struct {
	AppPath string
	Keys    []byte
}
type ResponseQuery interface{}

type RequestCheckTx struct {
	Tx   []byte      `protobuf:"bytes,1,opt,name=tx,proto3" json:"tx,omitempty"`
	Type CheckTxType `protobuf:"varint,2,opt,name=type,proto3,enum=types.CheckTxType" json:"type,omitempty"`
}

type ResponseCheckTx interface {
	IsOK() bool
}

type CheckTxType int32
