package server

import (
	"context"
	"encoding/json"
	"kimo/types"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/cenkalti/log"
)

// KimoServerResponse is type for returning a response from kimo server
type KimoServerResponse struct {
	Processes []Process `json:"processes"`
}

// Process is the final processes that is combined with DaemonProcess + TCPProxyRecord + MysqlProcess
type Process struct {
	ID        int32    `json:"id"`
	MysqlUser string   `json:"mysql_user"`
	DB        string   `json:"db"`
	Command   string   `json:"command"`
	Time      uint32   `json:"time"`
	State     string   `json:"state"`
	Info      string   `json:"info"`
	CmdLine   []string `json:"cmdline"`
	Pid       int32    `json:"pid"`
	Host      string   `json:"host"`
}

// KimoRequest is used to represent a new request to the server
type KimoRequest struct {
	Mysql         *Mysql
	Server        *Server
	TCPProxy      *TCPProxy
	DaemonPort    uint32
	KimoProcesses []*KimoProcess
}

// NewKimoRequest is used to create a new KimoRequest
func (s *Server) NewKimoRequest() *KimoRequest {
	kr := new(KimoRequest)
	kr.Mysql = NewMysql(s.Config.DSN)
	kr.TCPProxy = NewTCPProxy(s.Config.TCPProxyMgmtAddress, s.Config.TCPProxyConnectTimeout, s.Config.TCPProxyReadTimeout)
	kr.DaemonPort = s.Config.DaemonPort
	kr.KimoProcesses = make([]*KimoProcess, 0)
	kr.Server = s
	return kr
}

// InitializeKimoProcesses initialize kimo processes combined with tcpproxy records and mysql processes
func (kr *KimoRequest) InitializeKimoProcesses(mps []*MysqlProcess, tprs []*TCPProxyRecord) error {
	for _, mp := range mps {
		tpr := kr.findTCPProxyRecord(mp.Address, tprs)
		if tpr == nil {
			continue
		}
		kr.KimoProcesses = append(kr.KimoProcesses, &KimoProcess{
			MysqlProcess:   mp,
			TCPProxyRecord: tpr,
			KimoRequest:    kr,
			Server:         kr.Server,
		})
	}
	return nil
}

func findHostIP(host string) (string, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil {
			return "", err
		}
		return string(ips[0].String()), nil
	}
	return ip.String(), nil
}

func (kr *KimoRequest) findTCPProxyRecord(addr types.IPPort, proxyRecords []*TCPProxyRecord) *TCPProxyRecord {
	ipAddr, err := findHostIP(addr.IP)
	if err != nil {
		log.Debugln(err.Error())
		return nil
	}

	for _, pr := range proxyRecords {
		if pr.ProxyOut.IP == ipAddr && pr.ProxyOut.Port == addr.Port {
			return pr
		}
	}
	return nil
}

// Setup is used for setting up kimo processes with fetching information from mysql and tcpproxy
func (kr *KimoRequest) Setup(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errC := make(chan error)

	mysqlProcsC := make(chan []*MysqlProcess)
	proxyRecordsC := make(chan []*TCPProxyRecord)

	var mysqlProcesses []*MysqlProcess
	var tcpProxyRecords []*TCPProxyRecord

	go kr.Mysql.FetchProcesses(ctx, mysqlProcsC, errC)
	go kr.TCPProxy.FetchRecords(ctx, proxyRecordsC, errC)
	for {
		if mysqlProcesses != nil && tcpProxyRecords != nil {
			return kr.InitializeKimoProcesses(mysqlProcesses, tcpProxyRecords)
		}
		select {
		case mps := <-mysqlProcsC:
			mysqlProcesses = mps
		case tprs := <-proxyRecordsC:
			tcpProxyRecords = tprs
		case err := <-errC:
			log.Errorf("Error occured: %s", err.Error())
			cancel()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

}

// GenerateKimoProcesses is used to combine all information (mysql + tcpproxy + daemon) of a process
func (kr *KimoRequest) GenerateKimoProcesses(ctx context.Context) {
	log.Infof("Generating %d kimo processes...\n", len(kr.KimoProcesses))
	var wg sync.WaitGroup
	for _, kp := range kr.KimoProcesses {
		wg.Add(1)
		go kp.SetDaemonProcess(ctx, &wg)
	}
	wg.Wait()
}

// ReturnResponse is used to return a response from server
func (kr *KimoRequest) ReturnResponse(ctx context.Context, w http.ResponseWriter) {
	log.Infof("Returning response with %d kimo processes...\n", len(kr.KimoProcesses))
	processes := make([]Process, 0)
	for _, kp := range kr.KimoProcesses {

		ut, err := strconv.ParseUint(kp.MysqlProcess.Time, 10, 32)
		if err != nil {
			log.Errorf("time %s could not be converted to int", kp.MysqlProcess.Time)
		}
		t := uint32(ut)

		processes = append(processes, Process{
			ID:        kp.MysqlProcess.ID,
			MysqlUser: kp.MysqlProcess.User,
			DB:        kp.MysqlProcess.DB.String,
			Command:   kp.MysqlProcess.Command,
			Time:      t,
			State:     kp.MysqlProcess.State.String,
			Info:      kp.MysqlProcess.Info.String,
			CmdLine:   kp.DaemonProcess.CmdLine,
			Pid:       kp.DaemonProcess.Pid,
			Host:      kp.DaemonProcess.Hostname,
		})
	}

	w.Header().Set("Content-Type", "application/json")

	response := &KimoServerResponse{
		Processes: processes,
	}
	json.NewEncoder(w).Encode(response)
}
