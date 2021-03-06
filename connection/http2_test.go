package connection

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	tunnelpogs "github.com/cloudflare/cloudflared/tunnelrpc/pogs"

	"github.com/gobwas/ws/wsutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

var (
	testTransport = http2.Transport{}
)

func newTestHTTP2Connection() (*http2Connection, net.Conn) {
	edgeConn, originConn := net.Pipe()
	var connIndex = uint8(0)
	return NewHTTP2Connection(
		originConn,
		testConfig,
		&NamedTunnelConfig{},
		&pogs.ConnectionOptions{},
		testObserver,
		connIndex,
		mockConnectedFuse{},
		nil,
	), edgeConn
}

func TestServeHTTP(t *testing.T) {
	tests := []testRequest{
		{
			name:           "ok",
			endpoint:       "ok",
			expectedStatus: http.StatusOK,
			expectedBody:   []byte(http.StatusText(http.StatusOK)),
		},
		{
			name:           "large_file",
			endpoint:       "large_file",
			expectedStatus: http.StatusOK,
			expectedBody:   testLargeResp,
		},
		{
			name:           "Bad request",
			endpoint:       "400",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   []byte(http.StatusText(http.StatusBadRequest)),
		},
		{
			name:           "Internal server error",
			endpoint:       "500",
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   []byte(http.StatusText(http.StatusInternalServerError)),
		},
		{
			name:           "Proxy error",
			endpoint:       "error",
			expectedStatus: http.StatusBadGateway,
			expectedBody:   nil,
			isProxyError:   true,
		},
	}

	http2Conn, edgeConn := newTestHTTP2Connection()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.Serve(ctx)
	}()

	edgeHTTP2Conn, err := testTransport.NewClientConn(edgeConn)
	require.NoError(t, err)

	for _, test := range tests {
		endpoint := fmt.Sprintf("http://localhost:8080/%s", test.endpoint)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		require.NoError(t, err)

		resp, err := edgeHTTP2Conn.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, test.expectedStatus, resp.StatusCode)
		if test.expectedBody != nil {
			respBody, err := ioutil.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, test.expectedBody, respBody)
		}
		if test.isProxyError {
			require.Equal(t, responseMetaHeaderCfd, resp.Header.Get(ResponseMetaHeaderField))
		} else {
			require.Equal(t, responseMetaHeaderOrigin, resp.Header.Get(ResponseMetaHeaderField))
		}
	}
	cancel()
	wg.Wait()
}

type mockNamedTunnelRPCClient struct {
	registered   chan struct{}
	unregistered chan struct{}
}

func (mc mockNamedTunnelRPCClient) RegisterConnection(
	c context.Context,
	config *NamedTunnelConfig,
	options *tunnelpogs.ConnectionOptions,
	connIndex uint8,
	observer *Observer,
) error {
	close(mc.registered)
	return nil
}

func (mc mockNamedTunnelRPCClient) GracefulShutdown(ctx context.Context, gracePeriod time.Duration) {
	close(mc.unregistered)
}

func (mockNamedTunnelRPCClient) Close() {}

type mockRPCClientFactory struct {
	registered   chan struct{}
	unregistered chan struct{}
}

func (mf *mockRPCClientFactory) newMockRPCClient(context.Context, io.ReadWriteCloser, *zerolog.Logger) NamedTunnelRPCClient {
	return mockNamedTunnelRPCClient{
		registered:   mf.registered,
		unregistered: mf.unregistered,
	}
}

type wsRespWriter struct {
	*httptest.ResponseRecorder
	readPipe  *io.PipeReader
	writePipe *io.PipeWriter
}

func newWSRespWriter() *wsRespWriter {
	readPipe, writePipe := io.Pipe()
	return &wsRespWriter{
		httptest.NewRecorder(),
		readPipe,
		writePipe,
	}
}

func (w *wsRespWriter) RespBody() io.ReadWriter {
	return nowriter{w.readPipe}
}

func (w *wsRespWriter) Write(data []byte) (n int, err error) {
	return w.writePipe.Write(data)
}

func TestServeWS(t *testing.T) {
	http2Conn, _ := newTestHTTP2Connection()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.Serve(ctx)
	}()

	respWriter := newWSRespWriter()
	readPipe, writePipe := io.Pipe()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8080/ws", readPipe)
	require.NoError(t, err)
	req.Header.Set(internalUpgradeHeader, websocketUpgrade)

	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.ServeHTTP(respWriter, req)
	}()

	data := []byte("test websocket")
	err = wsutil.WriteClientText(writePipe, data)
	require.NoError(t, err)

	respBody, err := wsutil.ReadServerText(respWriter.RespBody())
	require.NoError(t, err)
	require.Equal(t, data, respBody, fmt.Sprintf("Expect %s, got %s", string(data), string(respBody)))

	cancel()
	resp := respWriter.Result()
	// http2RespWriter should rewrite status 101 to 200
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, responseMetaHeaderOrigin, resp.Header.Get(ResponseMetaHeaderField))

	wg.Wait()
}

func TestServeControlStream(t *testing.T) {
	http2Conn, edgeConn := newTestHTTP2Connection()

	rpcClientFactory := mockRPCClientFactory{
		registered:   make(chan struct{}),
		unregistered: make(chan struct{}),
	}
	http2Conn.newRPCClientFunc = rpcClientFactory.newMockRPCClient

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.Serve(ctx)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8080/", nil)
	require.NoError(t, err)
	req.Header.Set(internalUpgradeHeader, controlStreamUpgrade)

	edgeHTTP2Conn, err := testTransport.NewClientConn(edgeConn)
	require.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		edgeHTTP2Conn.RoundTrip(req)
	}()

	<-rpcClientFactory.registered
	cancel()
	<-rpcClientFactory.unregistered
	assert.False(t, http2Conn.stoppedGracefully)

	wg.Wait()
}

func TestGracefulShutdownHTTP2(t *testing.T) {
	http2Conn, edgeConn := newTestHTTP2Connection()

	rpcClientFactory := mockRPCClientFactory{
		registered:   make(chan struct{}),
		unregistered: make(chan struct{}),
	}
	http2Conn.newRPCClientFunc = rpcClientFactory.newMockRPCClient
	http2Conn.gracefulShutdownC = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.Serve(ctx)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8080/", nil)
	require.NoError(t, err)
	req.Header.Set(internalUpgradeHeader, controlStreamUpgrade)

	edgeHTTP2Conn, err := testTransport.NewClientConn(edgeConn)
	require.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = edgeHTTP2Conn.RoundTrip(req)
	}()

	select {
	case <-rpcClientFactory.registered:
		break //ok
	case <-time.Tick(time.Second):
		t.Fatal("timeout out waiting for registration")
	}

	// signal graceful shutdown
	close(http2Conn.gracefulShutdownC)

	select {
	case <-rpcClientFactory.unregistered:
		break //ok
	case <-time.Tick(time.Second):
		t.Fatal("timeout out waiting for unregistered signal")
	}
	assert.True(t, http2Conn.stoppedGracefully)

	cancel()
	wg.Wait()
}

func benchmarkServeHTTP(b *testing.B, test testRequest) {
	http2Conn, edgeConn := newTestHTTP2Connection()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		http2Conn.Serve(ctx)
	}()

	endpoint := fmt.Sprintf("http://localhost:8080/%s", test.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	require.NoError(b, err)

	edgeHTTP2Conn, err := testTransport.NewClientConn(edgeConn)
	require.NoError(b, err)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		resp, err := edgeHTTP2Conn.RoundTrip(req)
		b.StopTimer()
		require.NoError(b, err)
		require.Equal(b, test.expectedStatus, resp.StatusCode)
		if test.expectedBody != nil {
			respBody, err := ioutil.ReadAll(resp.Body)
			require.NoError(b, err)
			require.Equal(b, test.expectedBody, respBody)
		}
		resp.Body.Close()
	}

	cancel()
	wg.Wait()
}

func BenchmarkServeHTTPSimple(b *testing.B) {
	test := testRequest{
		name:           "ok",
		endpoint:       "ok",
		expectedStatus: http.StatusOK,
		expectedBody:   []byte(http.StatusText(http.StatusOK)),
	}

	benchmarkServeHTTP(b, test)
}

func BenchmarkServeHTTPLargeFile(b *testing.B) {
	test := testRequest{
		name:           "large_file",
		endpoint:       "large_file",
		expectedStatus: http.StatusOK,
		expectedBody:   testLargeResp,
	}

	benchmarkServeHTTP(b, test)
}
