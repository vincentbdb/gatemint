package appinterface

type Application interface {
	Query(QueryParam) ResponseQuery // Query for state

}

type QueryParam struct {
	AppPath string
	Keys    []byte
}
type ResponseQuery interface{}
