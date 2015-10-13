package state

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/consul/consul/structs"
)

func testStateStore(t *testing.T) *StateStore {
	s, err := NewStateStore(os.Stderr)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if s == nil {
		t.Fatalf("missing state store")
	}
	return s
}

func testRegisterNode(t *testing.T, s *StateStore, idx uint64, nodeID string) {
	node := &structs.Node{Node: nodeID}
	if err := s.EnsureNode(idx, node); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()
	n, err := tx.First("nodes", "id", nodeID)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result, ok := n.(*structs.Node); !ok || result.Node != nodeID {
		t.Fatalf("bad node: %#v", result)
	}
}

func testRegisterService(t *testing.T, s *StateStore, idx uint64, nodeID, serviceID string) {
	svc := &structs.NodeService{
		ID:      serviceID,
		Service: serviceID,
		Address: "1.1.1.1",
		Port:    1111,
	}
	if err := s.EnsureService(idx, nodeID, svc); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()
	service, err := tx.First("services", "id", nodeID, serviceID)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result, ok := service.(*structs.ServiceNode); !ok ||
		result.Node != nodeID ||
		result.ServiceID != serviceID {
		t.Fatalf("bad service: %#v", result)
	}
}

func testRegisterCheck(t *testing.T, s *StateStore, idx uint64,
	nodeID, serviceID, checkID, state string) {
	chk := &structs.HealthCheck{
		Node:      nodeID,
		CheckID:   checkID,
		ServiceID: serviceID,
		Status:    state,
	}
	if err := s.EnsureCheck(idx, chk); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()
	c, err := tx.First("checks", "id", nodeID, checkID)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result, ok := c.(*structs.HealthCheck); !ok ||
		result.Node != nodeID ||
		result.ServiceID != serviceID ||
		result.CheckID != checkID {
		t.Fatalf("bad check: %#v", result)
	}
}

func testSetKey(t *testing.T, s *StateStore, idx uint64, key, value string) {
	entry := &structs.DirEntry{Key: key, Value: []byte(value)}
	if err := s.KVSSet(idx, entry); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()
	e, err := tx.First("kvs", "id", key)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result, ok := e.(*structs.DirEntry); !ok || result.Key != key {
		t.Fatalf("bad kvs entry: %#v", result)
	}
}

// verifyWatch will set up a watch channel, call the given function, and then
// make sure the watch fires within a fixed time period.
func verifyWatch(t *testing.T, watch Watch, fn func()) {
	ch := make(chan struct{})
	watch.Wait(ch)
	go fn()

	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatalf("watch was not notified in time")
	}
}

func TestStateStore_maxIndex(t *testing.T) {
	s := testStateStore(t)

	testRegisterNode(t, s, 0, "foo")
	testRegisterNode(t, s, 1, "bar")
	testRegisterService(t, s, 2, "foo", "consul")

	if max := s.maxIndex("nodes", "services"); max != 2 {
		t.Fatalf("bad max: %d", max)
	}
}

func TestStateStore_indexUpdateMaxTxn(t *testing.T) {
	s := testStateStore(t)

	testRegisterNode(t, s, 0, "foo")
	testRegisterNode(t, s, 1, "bar")

	tx := s.db.Txn(true)
	if err := indexUpdateMaxTxn(tx, 3, "nodes"); err != nil {
		t.Fatalf("err: %s", err)
	}
	tx.Commit()

	if max := s.maxIndex("nodes"); max != 3 {
		t.Fatalf("bad max: %d", max)
	}
}

func TestStateStore_ReapTombstones(t *testing.T) {
	s := testStateStore(t)

	// Create some KV pairs.
	testSetKey(t, s, 1, "foo", "foo")
	testSetKey(t, s, 2, "foo/bar", "bar")
	testSetKey(t, s, 3, "foo/baz", "bar")
	testSetKey(t, s, 4, "foo/moo", "bar")
	testSetKey(t, s, 5, "foo/zoo", "bar")

	// Call a delete on some specific keys.
	if err := s.KVSDelete(6, "foo/baz"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := s.KVSDelete(7, "foo/moo"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Pull out the list and check the index, which should come from the
	// tombstones.
	idx, _, err := s.KVSList("foo/")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 7 {
		t.Fatalf("bad index: %d", idx)
	}

	// Reap the tombstones <= 6.
	if err := s.ReapTombstones(6); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Should still be good because 7 is in there.
	idx, _, err = s.KVSList("foo/")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 7 {
		t.Fatalf("bad index: %d", idx)
	}

	// Now reap them all.
	if err := s.ReapTombstones(7); err != nil {
		t.Fatalf("err: %s", err)
	}

	// At this point the index will slide backwards.
	idx, _, err = s.KVSList("foo/")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 5 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_EnsureRegistration(t *testing.T) {
	s := testStateStore(t)

	// Start with just a node.
	req := &structs.RegisterRequest{
		Node:    "node1",
		Address: "1.2.3.4",
	}
	if err := s.EnsureRegistration(1, req); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the node and verify its contents.
	verifyNode := func(created, modified uint64) {
		out, err := s.GetNode("node1")
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if out.Node != "node1" || out.Address != "1.2.3.4" ||
			out.CreateIndex != created || out.ModifyIndex != modified {
			t.Fatalf("bad node returned: %#v", out)
		}
	}
	verifyNode(1, 1)

	// Add in a service definition.
	req.Service = &structs.NodeService{
		ID:      "redis1",
		Service: "redis",
		Address: "1.1.1.1",
		Port:    8080,
	}
	if err := s.EnsureRegistration(2, req); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Verify that the service got registered.
	verifyService := func(created, modified uint64) {
		idx, out, err := s.NodeServices("node1")
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if idx != modified {
			t.Fatalf("bad index: %d", idx)
		}
		if len(out.Services) != 1 {
			t.Fatalf("bad: %#v", out.Services)
		}
		s := out.Services["redis1"]
		if s.ID != "redis1" || s.Service != "redis" ||
			s.Address != "1.1.1.1" || s.Port != 8080 ||
			s.CreateIndex != created || s.ModifyIndex != modified {
			t.Fatalf("bad service returned: %#v", s)
		}
	}
	verifyNode(1, 2)
	verifyService(2, 2)

	// Add in a top-level check.
	req.Check = &structs.HealthCheck{
		Node:    "node1",
		CheckID: "check1",
		Name:    "check",
	}
	if err := s.EnsureRegistration(3, req); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Verify that the check got registered.
	verifyCheck := func(created, modified uint64) {
		idx, out, err := s.NodeChecks("node1")
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if idx != modified {
			t.Fatalf("bad index: %d", idx)
		}
		if len(out) != 1 {
			t.Fatalf("bad: %#v", out)
		}
		c := out[0]
		if c.Node != "node1" || c.CheckID != "check1" || c.Name != "check" ||
			c.CreateIndex != created || c.ModifyIndex != modified {
			t.Fatalf("bad check returned: %#v", c)
		}
	}
	verifyNode(1, 3)
	verifyService(2, 3)
	verifyCheck(3, 3)

	// Add in another check via the slice.
	req.Checks = structs.HealthChecks{
		&structs.HealthCheck{
			Node:      "node1",
			CheckID:   "check2",
			Name:      "check",
		},
	}
	if err := s.EnsureRegistration(4, req); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Verify that the additional check got registered.
	verifyNode(1, 4)
	verifyService(2, 4)
	func() {
		idx, out, err := s.NodeChecks("node1")
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if idx != 4 {
			t.Fatalf("bad index: %d", idx)
		}
		if len(out) != 2 {
			t.Fatalf("bad: %#v", out)
		}
		c1 := out[0]
		if c1.Node != "node1" || c1.CheckID != "check1" || c1.Name != "check" ||
			c1.CreateIndex != 3 || c1.ModifyIndex != 4 {
			t.Fatalf("bad check returned: %#v", c1)
		}

		c2 := out[1]
		if c2.Node != "node1" || c2.CheckID != "check2" || c2.Name != "check" ||
			c2.CreateIndex != 4 || c2.ModifyIndex != 4 {
			t.Fatalf("bad check returned: %#v", c2)
		}
	}()
}

func TestStateStore_EnsureNode(t *testing.T) {
	s := testStateStore(t)

	// Fetching a non-existent node returns nil
	if node, err := s.GetNode("node1"); node != nil || err != nil {
		t.Fatalf("expected (nil, nil), got: (%#v, %#v)", node, err)
	}

	// Create a node registration request
	in := &structs.Node{
		Node:    "node1",
		Address: "1.1.1.1",
	}

	// Ensure the node is registered in the db
	if err := s.EnsureNode(1, in); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the node again
	out, err := s.GetNode("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Correct node was returned
	if out.Node != "node1" || out.Address != "1.1.1.1" {
		t.Fatalf("bad node returned: %#v", out)
	}

	// Indexes are set properly
	if out.CreateIndex != 1 || out.ModifyIndex != 1 {
		t.Fatalf("bad node index: %#v", out)
	}

	// Update the node registration
	in.Address = "1.1.1.2"
	if err := s.EnsureNode(2, in); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the node
	out, err = s.GetNode("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Node and indexes were updated
	if out.CreateIndex != 1 || out.ModifyIndex != 2 || out.Address != "1.1.1.2" {
		t.Fatalf("bad: %#v", out)
	}

	// Node upsert is idempotent
	if err := s.EnsureNode(2, in); err != nil {
		t.Fatalf("err: %s", err)
	}
	out, err = s.GetNode("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if out.Address != "1.1.1.2" || out.CreateIndex != 1 || out.ModifyIndex != 2 {
		t.Fatalf("node was modified: %#v", out)
	}

	// Index tables were updated
	if idx := s.maxIndex("nodes"); idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_GetNodes(t *testing.T) {
	s := testStateStore(t)

	// Listing with no results returns nil
	idx, res, err := s.Nodes()
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Create some nodes in the state store
	testRegisterNode(t, s, 0, "node0")
	testRegisterNode(t, s, 1, "node1")
	testRegisterNode(t, s, 2, "node2")

	// Retrieve the nodes
	idx, nodes, err := s.Nodes()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Highest index was returned
	if idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}

	// All nodes were returned
	if n := len(nodes); n != 3 {
		t.Fatalf("bad node count: %d", n)
	}

	// Make sure the nodes match
	for i, node := range nodes {
		if node.CreateIndex != uint64(i) || node.ModifyIndex != uint64(i) {
			t.Fatalf("bad node index: %d, %d", node.CreateIndex, node.ModifyIndex)
		}
		name := fmt.Sprintf("node%d", i)
		if node.Node != name {
			t.Fatalf("bad: %#v", node)
		}
	}
}

func TestStateStore_DeleteNode(t *testing.T) {
	s := testStateStore(t)

	// Create a node and register a service and health check with it.
	testRegisterNode(t, s, 0, "node1")
	testRegisterService(t, s, 1, "node1", "service1")
	testRegisterCheck(t, s, 2, "node1", "", "check1", structs.HealthPassing)

	// Delete the node
	if err := s.DeleteNode(3, "node1"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// The node was removed
	if n, err := s.GetNode("node1"); err != nil || n != nil {
		t.Fatalf("bad: %#v (err: %#v)", n, err)
	}

	// Associated service was removed. Need to query this directly out of
	// the DB to make sure it is actually gone.
	tx := s.db.Txn(false)
	defer tx.Abort()
	services, err := tx.Get("services", "id", "node1", "service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if s := services.Next(); s != nil {
		t.Fatalf("bad: %#v", s)
	}

	// Associated health check was removed.
	checks, err := tx.Get("checks", "id", "node1", "check1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if c := checks.Next(); c != nil {
		t.Fatalf("bad: %#v", c)
	}

	// Indexes were updated.
	for _, tbl := range []string{"nodes", "services", "checks"} {
		if idx := s.maxIndex(tbl); idx != 3 {
			t.Fatalf("bad index: %d (%s)", idx, tbl)
		}
	}

	// Deleting a nonexistent node should be idempotent and not return
	// an error
	if err := s.DeleteNode(4, "node1"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("nodes"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_EnsureService(t *testing.T) {
	s := testStateStore(t)

	// Fetching services for a node with none returns nil
	idx, res, err := s.NodeServices("node1")
	if err != nil || res != nil || idx != 0 {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Create the service registration
	ns1 := &structs.NodeService{
		ID:      "service1",
		Service: "redis",
		Tags:    []string{"prod"},
		Address: "1.1.1.1",
		Port:    1111,
	}

	// Creating a service without a node returns an error
	if err := s.EnsureService(1, "node1", ns1); err != ErrMissingNode {
		t.Fatalf("expected %#v, got: %#v", ErrMissingNode, err)
	}

	// Register the nodes
	testRegisterNode(t, s, 0, "node1")
	testRegisterNode(t, s, 1, "node2")

	// Service successfully registers into the state store
	if err = s.EnsureService(10, "node1", ns1); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Register a similar service against both nodes
	ns2 := *ns1
	ns2.ID = "service2"
	for _, n := range []string{"node1", "node2"} {
		if err := s.EnsureService(20, n, &ns2); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// Register a different service on the bad node
	ns3 := *ns1
	ns3.ID = "service3"
	if err := s.EnsureService(30, "node2", &ns3); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the services
	idx, out, err := s.NodeServices("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Highest index for the result set was returned
	if idx != 20 {
		t.Fatalf("bad index: %d", idx)
	}

	// Only the services for the requested node are returned
	if out == nil || len(out.Services) != 2 {
		t.Fatalf("bad services: %#v", out)
	}

	// Results match the inserted services and have the proper indexes set
	expect1 := *ns1
	expect1.CreateIndex, expect1.ModifyIndex = 10, 10
	if svc := out.Services["service1"]; !reflect.DeepEqual(&expect1, svc) {
		t.Fatalf("bad: %#v", svc)
	}

	expect2 := ns2
	expect2.CreateIndex, expect2.ModifyIndex = 20, 20
	if svc := out.Services["service2"]; !reflect.DeepEqual(&expect2, svc) {
		t.Fatalf("bad: %#v %#v", ns2, svc)
	}

	// Index tables were updated
	if idx := s.maxIndex("services"); idx != 30 {
		t.Fatalf("bad index: %d", idx)
	}

	// Update a service registration
	ns1.Address = "1.1.1.2"
	if err := s.EnsureService(40, "node1", ns1); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the service again and ensure it matches
	idx, out, err = s.NodeServices("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 40 {
		t.Fatalf("bad index: %d", idx)
	}
	if out == nil || len(out.Services) != 2 {
		t.Fatalf("bad: %#v", out)
	}
	expect1.Address = "1.1.1.2"
	expect1.ModifyIndex = 40
	if svc := out.Services["service1"]; !reflect.DeepEqual(&expect1, svc) {
		t.Fatalf("bad: %#v", svc)
	}

	// Index tables were updated
	if idx := s.maxIndex("services"); idx != 40 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_DeleteService(t *testing.T) {
	s := testStateStore(t)

	// Register a node with one service and a check
	testRegisterNode(t, s, 1, "node1")
	testRegisterService(t, s, 2, "node1", "service1")
	testRegisterCheck(t, s, 3, "node1", "service1", "check1", structs.HealthPassing)

	// Delete the service
	if err := s.DeleteService(4, "node1", "service1"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Service doesn't exist.
	_, ns, err := s.NodeServices("node1")
	if err != nil || ns == nil || len(ns.Services) != 0 {
		t.Fatalf("bad: %#v (err: %#v)", ns, err)
	}

	// Check doesn't exist. Check using the raw DB so we can test
	// that it actually is removed in the state store.
	tx := s.db.Txn(false)
	defer tx.Abort()
	check, err := tx.First("checks", "id", "node1", "check1")
	if err != nil || check != nil {
		t.Fatalf("bad: %#v (err: %s)", check, err)
	}

	// Index tables were updated
	if idx := s.maxIndex("services"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
	if idx := s.maxIndex("checks"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}

	// Deleting a nonexistent service should be idempotent and not return an
	// error
	if err := s.DeleteService(5, "node1", "service1"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("services"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_EnsureCheck(t *testing.T) {
	s := testStateStore(t)

	// Create a check associated with the node
	check := &structs.HealthCheck{
		Node:        "node1",
		CheckID:     "check1",
		Name:        "redis check",
		Status:      structs.HealthPassing,
		Notes:       "test check",
		Output:      "aaa",
		ServiceID:   "service1",
		ServiceName: "redis",
	}

	// Creating a check without a node returns error
	if err := s.EnsureCheck(1, check); err != ErrMissingNode {
		t.Fatalf("expected %#v, got: %#v", ErrMissingNode, err)
	}

	// Register the node
	testRegisterNode(t, s, 1, "node1")

	// Creating a check with a bad services returns error
	if err := s.EnsureCheck(1, check); err != ErrMissingService {
		t.Fatalf("expected: %#v, got: %#v", ErrMissingService, err)
	}

	// Register the service
	testRegisterService(t, s, 2, "node1", "service1")

	// Inserting the check with the prerequisites succeeds
	if err := s.EnsureCheck(3, check); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the check and make sure it matches
	idx, checks, err := s.NodeChecks("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(checks) != 1 {
		t.Fatalf("wrong number of checks: %d", len(checks))
	}
	if !reflect.DeepEqual(checks[0], check) {
		t.Fatalf("bad: %#v", checks[0])
	}

	// Modify the health check
	check.Output = "bbb"
	if err := s.EnsureCheck(4, check); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that we successfully updated
	idx, checks, err = s.NodeChecks("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(checks) != 1 {
		t.Fatalf("wrong number of checks: %d", len(checks))
	}
	if checks[0].Output != "bbb" {
		t.Fatalf("wrong check output: %#v", checks[0])
	}
	if checks[0].CreateIndex != 3 || checks[0].ModifyIndex != 4 {
		t.Fatalf("bad index: %#v", checks[0])
	}

	// Index tables were updated
	if idx := s.maxIndex("checks"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_EnsureCheck_defaultStatus(t *testing.T) {
	s := testStateStore(t)

	// Register a node
	testRegisterNode(t, s, 1, "node1")

	// Create and register a check with no health status
	check := &structs.HealthCheck{
		Node:    "node1",
		CheckID: "check1",
		Status:  "",
	}
	if err := s.EnsureCheck(2, check); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Get the check again
	_, result, err := s.NodeChecks("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that the status was set to the proper default
	if len(result) != 1 || result[0].Status != structs.HealthCritical {
		t.Fatalf("bad: %#v", result)
	}
}

func TestStateStore_NodeChecks(t *testing.T) {
	s := testStateStore(t)

	// Create the first node and service with some checks
	testRegisterNode(t, s, 0, "node1")
	testRegisterService(t, s, 1, "node1", "service1")
	testRegisterCheck(t, s, 2, "node1", "service1", "check1", structs.HealthPassing)
	testRegisterCheck(t, s, 3, "node1", "service1", "check2", structs.HealthPassing)

	// Create a second node/service with a different set of checks
	testRegisterNode(t, s, 4, "node2")
	testRegisterService(t, s, 5, "node2", "service2")
	testRegisterCheck(t, s, 6, "node2", "service2", "check3", structs.HealthPassing)

	// Try querying for all checks associated with node1
	idx, checks, err := s.NodeChecks("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(checks) != 2 || checks[0].CheckID != "check1" || checks[1].CheckID != "check2" {
		t.Fatalf("bad checks: %#v", checks)
	}

	// Try querying for all checks associated with node2
	idx, checks, err = s.NodeChecks("node2")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 6 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(checks) != 1 || checks[0].CheckID != "check3" {
		t.Fatalf("bad checks: %#v", checks)
	}
}

func TestStateStore_ServiceChecks(t *testing.T) {
	s := testStateStore(t)

	// Create the first node and service with some checks
	testRegisterNode(t, s, 0, "node1")
	testRegisterService(t, s, 1, "node1", "service1")
	testRegisterCheck(t, s, 2, "node1", "service1", "check1", structs.HealthPassing)
	testRegisterCheck(t, s, 3, "node1", "service1", "check2", structs.HealthPassing)

	// Create a second node/service with a different set of checks
	testRegisterNode(t, s, 4, "node2")
	testRegisterService(t, s, 5, "node2", "service2")
	testRegisterCheck(t, s, 6, "node2", "service2", "check3", structs.HealthPassing)

	// Try querying for all checks associated with service1
	idx, checks, err := s.ServiceChecks("service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(checks) != 2 || checks[0].CheckID != "check1" || checks[1].CheckID != "check2" {
		t.Fatalf("bad checks: %#v", checks)
	}
}

func TestStateStore_ChecksInState(t *testing.T) {
	s := testStateStore(t)

	// Querying with no results returns nil
	idx, res, err := s.ChecksInState(structs.HealthPassing)
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Register a node with checks in varied states
	testRegisterNode(t, s, 0, "node1")
	testRegisterCheck(t, s, 1, "node1", "", "check1", structs.HealthPassing)
	testRegisterCheck(t, s, 2, "node1", "", "check2", structs.HealthCritical)
	testRegisterCheck(t, s, 3, "node1", "", "check3", structs.HealthPassing)

	// Query the state store for passing checks.
	_, checks, err := s.ChecksInState(structs.HealthPassing)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Make sure we only get the checks which match the state
	if n := len(checks); n != 2 {
		t.Fatalf("expected 2 checks, got: %d", n)
	}
	if checks[0].CheckID != "check1" || checks[1].CheckID != "check3" {
		t.Fatalf("bad: %#v", checks)
	}

	// HealthAny just returns everything.
	_, checks, err = s.ChecksInState(structs.HealthAny)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if n := len(checks); n != 3 {
		t.Fatalf("expected 3 checks, got: %d", n)
	}
}

func TestStateStore_DeleteCheck(t *testing.T) {
	s := testStateStore(t)

	// Register a node and a node-level health check
	testRegisterNode(t, s, 1, "node1")
	testRegisterCheck(t, s, 2, "node1", "", "check1", structs.HealthPassing)

	// Delete the check
	if err := s.DeleteCheck(3, "node1", "check1"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check is gone
	_, checks, err := s.NodeChecks("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if len(checks) != 0 {
		t.Fatalf("bad: %#v", checks)
	}

	// Index tables were updated
	if idx := s.maxIndex("checks"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}

	// Deleting a nonexistent check should be idempotent and not return an
	// error
	if err := s.DeleteCheck(4, "node1", "check1"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("checks"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_CheckServiceNodes(t *testing.T) {
	s := testStateStore(t)

	// Querying with no matches gives an empty response
	idx, res, err := s.CheckServiceNodes("service1")
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Register some nodes
	testRegisterNode(t, s, 0, "node1")
	testRegisterNode(t, s, 1, "node2")

	// Register node-level checks. These should not be returned
	// in the final result.
	testRegisterCheck(t, s, 2, "node1", "", "check1", structs.HealthPassing)
	testRegisterCheck(t, s, 3, "node2", "", "check2", structs.HealthPassing)

	// Register a service against the nodes
	testRegisterService(t, s, 4, "node1", "service1")
	testRegisterService(t, s, 5, "node2", "service2")

	// Register checks against the services
	testRegisterCheck(t, s, 6, "node1", "service1", "check3", structs.HealthPassing)
	testRegisterCheck(t, s, 7, "node2", "service2", "check4", structs.HealthPassing)

	// Query the state store for nodes and checks which
	// have been registered with a specific service.
	idx, results, err := s.CheckServiceNodes("service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check the index returned matches the result set. The index
	// should be the highest observed from the result, in this case
	// this comes from the check registration.
	if idx != 6 {
		t.Fatalf("bad index: %d", idx)
	}

	// Make sure we get the expected result
	if n := len(results); n != 1 {
		t.Fatalf("expected 1 result, got: %d", n)
	}
	csn := results[0]
	if csn.Node == nil || csn.Service == nil || len(csn.Checks) != 1 {
		t.Fatalf("bad output: %#v", csn)
	}

	// Node updates alter the returned index
	testRegisterNode(t, s, 8, "node1")
	idx, results, err = s.CheckServiceNodes("service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 8 {
		t.Fatalf("bad index: %d", idx)
	}

	// Service updates alter the returned index
	testRegisterService(t, s, 9, "node1", "service1")
	idx, results, err = s.CheckServiceNodes("service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 9 {
		t.Fatalf("bad index: %d", idx)
	}

	// Check updates alter the returned index
	testRegisterCheck(t, s, 10, "node1", "service1", "check1", structs.HealthCritical)
	idx, results, err = s.CheckServiceNodes("service1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 10 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_NodeInfo_NodeDump(t *testing.T) {
	s := testStateStore(t)

	// Generating a node dump that matches nothing returns empty
	idx, dump, err := s.NodeInfo("node1")
	if idx != 0 || dump != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, dump, err)
	}
	idx, dump, err = s.NodeDump()
	if idx != 0 || dump != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, dump, err)
	}

	// Register some nodes
	testRegisterNode(t, s, 0, "node1")
	testRegisterNode(t, s, 1, "node2")

	// Register services against them
	testRegisterService(t, s, 2, "node1", "service1")
	testRegisterService(t, s, 3, "node1", "service2")
	testRegisterService(t, s, 4, "node2", "service1")
	testRegisterService(t, s, 5, "node2", "service2")

	// Register service-level checks
	testRegisterCheck(t, s, 6, "node1", "service1", "check1", structs.HealthPassing)
	testRegisterCheck(t, s, 7, "node2", "service1", "check1", structs.HealthPassing)

	// Register node-level checks
	testRegisterCheck(t, s, 8, "node1", "", "check2", structs.HealthPassing)
	testRegisterCheck(t, s, 9, "node2", "", "check2", structs.HealthPassing)

	// Check that our result matches what we expect.
	expect := structs.NodeDump{
		&structs.NodeInfo{
			Node: "node1",
			Checks: structs.HealthChecks{
				&structs.HealthCheck{
					Node:        "node1",
					CheckID:     "check1",
					ServiceID:   "service1",
					ServiceName: "service1",
					Status:      structs.HealthPassing,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 6,
						ModifyIndex: 6,
					},
				},
				&structs.HealthCheck{
					Node:        "node1",
					CheckID:     "check2",
					ServiceID:   "",
					ServiceName: "",
					Status:      structs.HealthPassing,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 8,
						ModifyIndex: 8,
					},
				},
			},
			Services: []*structs.NodeService{
				&structs.NodeService{
					ID:      "service1",
					Service: "service1",
					Address: "1.1.1.1",
					Port:    1111,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 2,
						ModifyIndex: 2,
					},
				},
				&structs.NodeService{
					ID:      "service2",
					Service: "service2",
					Address: "1.1.1.1",
					Port:    1111,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 3,
						ModifyIndex: 3,
					},
				},
			},
		},
		&structs.NodeInfo{
			Node: "node2",
			Checks: structs.HealthChecks{
				&structs.HealthCheck{
					Node:        "node2",
					CheckID:     "check1",
					ServiceID:   "service1",
					ServiceName: "service1",
					Status:      structs.HealthPassing,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 7,
						ModifyIndex: 7,
					},
				},
				&structs.HealthCheck{
					Node:        "node2",
					CheckID:     "check2",
					ServiceID:   "",
					ServiceName: "",
					Status:      structs.HealthPassing,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 9,
						ModifyIndex: 9,
					},
				},
			},
			Services: []*structs.NodeService{
				&structs.NodeService{
					ID:      "service1",
					Service: "service1",
					Address: "1.1.1.1",
					Port:    1111,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 4,
						ModifyIndex: 4,
					},
				},
				&structs.NodeService{
					ID:      "service2",
					Service: "service2",
					Address: "1.1.1.1",
					Port:    1111,
					RaftIndex: structs.RaftIndex{
						CreateIndex: 5,
						ModifyIndex: 5,
					},
				},
			},
		},
	}

	// Get a dump of just a single node
	idx, dump, err = s.NodeInfo("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 8 {
		t.Fatalf("bad index: %d", idx)
	}
	if len(dump) != 1 || !reflect.DeepEqual(dump[0], expect[0]) {
		t.Fatalf("bad: %#v", dump)
	}

	// Generate a dump of all the nodes
	idx, dump, err = s.NodeDump()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 9 {
		t.Fatalf("bad index: %d", 9)
	}
	if !reflect.DeepEqual(dump, expect) {
		t.Fatalf("bad: %#v", dump[0].Services[0])
	}
}

func TestStateStore_KVSSet_KVSGet(t *testing.T) {
	s := testStateStore(t)

	// Get on an nonexistent key returns nil
	result, err := s.KVSGet("foo")
	if result != nil || err != nil {
		t.Fatalf("expected (nil, nil), got : (%#v, %#v)", result, err)
	}

	// Write a new K/V entry to the store
	entry := &structs.DirEntry{
		Key:   "foo",
		Value: []byte("bar"),
	}
	if err := s.KVSSet(1, entry); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the K/V entry again
	result, err = s.KVSGet("foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result == nil {
		t.Fatalf("expected k/v pair, got nothing")
	}

	// Check that the index was injected into the result
	if result.CreateIndex != 1 || result.ModifyIndex != 1 {
		t.Fatalf("bad index: %d, %d", result.CreateIndex, result.ModifyIndex)
	}

	// Check that the value matches
	if v := string(result.Value); v != "bar" {
		t.Fatalf("expected 'bar', got: '%s'", v)
	}

	// Updating the entry works and changes the index
	update := &structs.DirEntry{
		Key:   "foo",
		Value: []byte("baz"),
	}
	if err := s.KVSSet(2, update); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Fetch the kv pair and check
	result, err = s.KVSGet("foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result.CreateIndex != 1 || result.ModifyIndex != 2 {
		t.Fatalf("bad index: %d, %d", result.CreateIndex, result.ModifyIndex)
	}
	if v := string(result.Value); v != "baz" {
		t.Fatalf("expected 'baz', got '%s'", v)
	}
}

func TestStateStore_KVSList(t *testing.T) {
	s := testStateStore(t)

	// Listing an empty KVS returns nothing
	idx, keys, err := s.KVSList("")
	if idx != 0 || keys != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, keys, err)
	}

	// Create some KVS entries
	testSetKey(t, s, 1, "foo", "foo")
	testSetKey(t, s, 2, "foo/bar", "bar")
	testSetKey(t, s, 3, "foo/bar/zip", "zip")
	testSetKey(t, s, 4, "foo/bar/zip/zorp", "zorp")
	testSetKey(t, s, 5, "foo/bar/baz", "baz")

	// List out all of the keys
	idx, keys, err = s.KVSList("")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check the index
	if idx != 5 {
		t.Fatalf("bad index: %d", idx)
	}

	// Check that all of the keys were returned
	if n := len(keys); n != 5 {
		t.Fatalf("expected 5 kvs entries, got: %d", n)
	}

	// Try listing with a provided prefix
	idx, keys, err = s.KVSList("foo/bar/zip")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}

	// Check that only the keys in the prefix were returned
	if n := len(keys); n != 2 {
		t.Fatalf("expected 2 kvs entries, got: %d", n)
	}
	if keys[0] != "foo/bar/zip" || keys[1] != "foo/bar/zip/zorp" {
		t.Fatalf("bad: %#v", keys)
	}
}

func TestStateStore_KVSListKeys(t *testing.T) {
	s := testStateStore(t)

	// Listing keys with no results returns nil
	idx, keys, err := s.KVSListKeys("", "")
	if idx != 0 || keys != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, keys, err)
	}

	// Create some keys
	testSetKey(t, s, 1, "foo", "foo")
	testSetKey(t, s, 2, "foo/bar", "bar")
	testSetKey(t, s, 3, "foo/bar/baz", "baz")
	testSetKey(t, s, 4, "foo/bar/zip", "zip")
	testSetKey(t, s, 5, "foo/bar/zip/zam", "zam")
	testSetKey(t, s, 6, "foo/bar/zip/zorp", "zorp")
	testSetKey(t, s, 7, "some/other/prefix", "nack")

	// Query using a prefix and pass a separator
	idx, keys, err = s.KVSListKeys("foo/bar/", "/")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 6 {
		t.Fatalf("bad index: %d", idx)
	}

	// Subset of the keys was returned
	expect := []string{"foo/bar/baz", "foo/bar/zip", "foo/bar/zip/"}
	if !reflect.DeepEqual(keys, expect) {
		t.Fatalf("bad keys: %#v", keys)
	}

	// Listing keys with no separator returns everything.
	idx, keys, err = s.KVSListKeys("foo", "")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 6 {
		t.Fatalf("bad index: %d", idx)
	}
	expect = []string{"foo", "foo/bar", "foo/bar/baz", "foo/bar/zip",
		"foo/bar/zip/zam", "foo/bar/zip/zorp"}
	if !reflect.DeepEqual(keys, expect) {
		t.Fatalf("bad keys: %#v", keys)
	}
}

func TestStateStore_KVSDelete(t *testing.T) {
	s := testStateStore(t)

	// Create some KV pairs
	testSetKey(t, s, 1, "foo", "foo")
	testSetKey(t, s, 2, "foo/bar", "bar")

	// Call a delete on a specific key
	if err := s.KVSDelete(3, "foo"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// The entry was removed from the state store
	tx := s.db.Txn(false)
	defer tx.Abort()
	e, err := tx.First("kvs", "id", "foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if e != nil {
		t.Fatalf("expected kvs entry to be deleted, got: %#v", e)
	}

	// Try fetching the other keys to ensure they still exist
	e, err = tx.First("kvs", "id", "foo/bar")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if e == nil || string(e.(*structs.DirEntry).Value) != "bar" {
		t.Fatalf("bad kvs entry: %#v", e)
	}

	// Check that the index table was updated
	if idx := s.maxIndex("kvs"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}

	// Deleting a nonexistent key should be idempotent and not return an
	// error
	if err := s.KVSDelete(4, "foo"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("kvs"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_KVSDeleteCAS(t *testing.T) {
	s := testStateStore(t)

	// Create some KV entries
	testSetKey(t, s, 1, "foo", "foo")
	testSetKey(t, s, 2, "bar", "bar")
	testSetKey(t, s, 3, "baz", "baz")

	// Do a CAS delete with an index lower than the entry
	ok, err := s.KVSDeleteCAS(4, 1, "bar")
	if ok || err != nil {
		t.Fatalf("expected (false, nil), got: (%v, %#v)", ok, err)
	}

	// Check that the index is untouched and the entry
	// has not been deleted.
	if idx := s.maxIndex("kvs"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
	e, err := s.KVSGet("foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if e == nil {
		t.Fatalf("expected a kvs entry, got nil")
	}

	// Do another CAS delete, this time with the correct index
	// which should cause the delete to take place.
	ok, err = s.KVSDeleteCAS(4, 2, "bar")
	if !ok || err != nil {
		t.Fatalf("expected (true, nil), got: (%v, %#v)", ok, err)
	}

	// Entry was deleted and index was updated
	if idx := s.maxIndex("kvs"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
	e, err = s.KVSGet("bar")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if e != nil {
		t.Fatalf("entry should be deleted")
	}

	// A delete on a nonexistent key should be idempotent and not return an
	// error
	ok, err = s.KVSDeleteCAS(5, 2, "bar")
	if !ok || err != nil {
		t.Fatalf("expected (true, nil), got: (%v, %#v)", ok, err)
	}
	if idx := s.maxIndex("kvs"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_KVSSetCAS(t *testing.T) {
	s := testStateStore(t)

	// Doing a CAS with ModifyIndex != 0 and no existing entry
	// is a no-op.
	entry := &structs.DirEntry{
		Key:   "foo",
		Value: []byte("foo"),
		RaftIndex: structs.RaftIndex{
			CreateIndex: 1,
			ModifyIndex: 1,
		},
	}
	ok, err := s.KVSSetCAS(2, entry)
	if ok || err != nil {
		t.Fatalf("expected (false, nil), got: (%#v, %#v)", ok, err)
	}

	// Check that nothing was actually stored
	tx := s.db.Txn(false)
	if e, err := tx.First("kvs", "id", "foo"); e != nil || err != nil {
		t.Fatalf("expected (nil, nil), got: (%#v, %#v)", e, err)
	}
	tx.Abort()

	// Index was not updated
	if idx := s.maxIndex("kvs"); idx != 0 {
		t.Fatalf("bad index: %d", idx)
	}

	// Doing a CAS with a ModifyIndex of zero when no entry exists
	// performs the set and saves into the state store.
	entry = &structs.DirEntry{
		Key:   "foo",
		Value: []byte("foo"),
		RaftIndex: structs.RaftIndex{
			CreateIndex: 0,
			ModifyIndex: 0,
		},
	}
	ok, err = s.KVSSetCAS(2, entry)
	if !ok || err != nil {
		t.Fatalf("expected (true, nil), got: (%#v, %#v)", ok, err)
	}

	// Entry was inserted
	tx = s.db.Txn(false)
	if e, err := tx.First("kvs", "id", "foo"); e == nil || err != nil || string(e.(*structs.DirEntry).Value) != "foo" {
		t.Fatalf("expected kvs to exist, got: (%#v, %#v)", e, err)
	}
	tx.Abort()

	// Index was updated
	if idx := s.maxIndex("kvs"); idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}

	// Doing a CAS with a ModifyIndex of zero when an entry exists does
	// not do anything.
	entry = &structs.DirEntry{
		Key:   "foo",
		Value: []byte("foo"),
		RaftIndex: structs.RaftIndex{
			CreateIndex: 0,
			ModifyIndex: 0,
		},
	}
	ok, err = s.KVSSetCAS(3, entry)
	if ok || err != nil {
		t.Fatalf("expected (false, nil), got: (%#v, %#v)", ok, err)
	}

	// Doing a CAS with a ModifyIndex which does not match the current
	// index does not do anything.
	entry = &structs.DirEntry{
		Key:   "foo",
		Value: []byte("bar"),
		RaftIndex: structs.RaftIndex{
			CreateIndex: 3,
			ModifyIndex: 3,
		},
	}
	ok, err = s.KVSSetCAS(3, entry)
	if ok || err != nil {
		t.Fatalf("expected (false, nil), got: (%#v, %#v)", ok, err)
	}

	// Entry was not updated in the store
	tx = s.db.Txn(false)
	e, err := tx.First("kvs", "id", "foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	result, ok := e.(*structs.DirEntry)
	if !ok || result.CreateIndex != 2 ||
		result.ModifyIndex != 2 || string(result.Value) != "foo" {
		t.Fatalf("bad: %#v", result)
	}

	// Index was not modified
	if idx := s.maxIndex("kvs"); idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}

	// Doing a CAS with the proper current index should make the
	// modification.
	entry = &structs.DirEntry{
		Key:   "foo",
		Value: []byte("bar"),
		RaftIndex: structs.RaftIndex{
			CreateIndex: 2,
			ModifyIndex: 2,
		},
	}
	ok, err = s.KVSSetCAS(3, entry)
	if !ok || err != nil {
		t.Fatalf("expected (true, nil), got: (%#v, %#v)", ok, err)
	}

	// Entry was updated
	tx = s.db.Txn(false)
	if e, err := tx.First("kvs", "id", "foo"); e == nil || err != nil || string(e.(*structs.DirEntry).Value) != "bar" {
		t.Fatalf("expected kvs to exist, got: (%#v, %#v)", e, err)
	}
	tx.Abort()

	// Index was updated
	if idx := s.maxIndex("kvs"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_KVSDeleteTree(t *testing.T) {
	s := testStateStore(t)

	// Create kvs entries in the state store
	testSetKey(t, s, 1, "foo/bar", "bar")
	testSetKey(t, s, 2, "foo/bar/baz", "baz")
	testSetKey(t, s, 3, "foo/bar/zip", "zip")
	testSetKey(t, s, 4, "foo/zorp", "zorp")

	// Calling tree deletion which affects nothing does not
	// modify the table index.
	if err := s.KVSDeleteTree(9, "bar"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("kvs"); idx != 4 {
		t.Fatalf("bad index: %d", idx)
	}

	// Call tree deletion with a nested prefix.
	if err := s.KVSDeleteTree(5, "foo/bar"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that all the matching keys were deleted
	tx := s.db.Txn(false)
	defer tx.Abort()

	entries, err := tx.Get("kvs", "id")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	num := 0
	for entry := entries.Next(); entry != nil; entry = entries.Next() {
		if entry.(*structs.DirEntry).Key != "foo/zorp" {
			t.Fatalf("unexpected kvs entry: %#v", entry)
		}
		num++
	}

	if num != 1 {
		t.Fatalf("expected 1 key, got: %d", num)
	}

	// Index should be updated if modifications are made
	if idx := s.maxIndex("kvs"); idx != 5 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_SessionCreate_GetSession(t *testing.T) {
	s := testStateStore(t)

	// GetSession returns nil if the session doesn't exist
	sess, err := s.GetSession("session1")
	if sess != nil || err != nil {
		t.Fatalf("expected (nil, nil), got: (%#v, %#v)", sess, err)
	}

	// Registering without a session ID is disallowed
	err = s.SessionCreate(1, &structs.Session{})
	if err != ErrMissingSessionID {
		t.Fatalf("expected %#v, got: %#v", ErrMissingSessionID, err)
	}

	// Invalid session behavior throws error
	sess = &structs.Session{
		ID:       "foo",
		Behavior: "nope",
	}
	err = s.SessionCreate(1, sess)
	if err == nil || !strings.Contains(err.Error(), "session behavior") {
		t.Fatalf("expected session behavior error, got: %#v", err)
	}

	// Registering with an unknown node is disallowed
	sess = &structs.Session{ID: "foo"}
	if err := s.SessionCreate(1, sess); err != ErrMissingNode {
		t.Fatalf("expected %#v, got: %#v", ErrMissingNode, err)
	}

	// None of the errored operations modified the index
	if idx := s.maxIndex("sessions"); idx != 0 {
		t.Fatalf("bad index: %d", idx)
	}

	// Valid session is able to register
	testRegisterNode(t, s, 1, "node1")
	sess = &structs.Session{
		ID:   "foo",
		Node: "node1",
	}
	if err := s.SessionCreate(2, sess); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("sessions"); idx != 2 {
		t.Fatalf("bad index: %s", err)
	}

	// Retrieve the session again
	session, err := s.GetSession("foo")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Ensure the session looks correct and was assigned the
	// proper default value for session behavior.
	expect := &structs.Session{
		ID:       "foo",
		Behavior: structs.SessionKeysRelease,
		Node:     "node1",
		RaftIndex: structs.RaftIndex{
			CreateIndex: 2,
			ModifyIndex: 2,
		},
	}
	if !reflect.DeepEqual(expect, session) {
		t.Fatalf("bad session: %#v", session)
	}

	// Registering with a non-existent check is disallowed
	sess = &structs.Session{
		ID:     "bar",
		Node:   "node1",
		Checks: []string{"check1"},
	}
	err = s.SessionCreate(3, sess)
	if err == nil || !strings.Contains(err.Error(), "Missing check") {
		t.Fatalf("expected missing check error, got: %#v", err)
	}

	// Registering with a critical check is disallowed
	testRegisterCheck(t, s, 3, "node1", "", "check1", structs.HealthCritical)
	err = s.SessionCreate(4, sess)
	if err == nil || !strings.Contains(err.Error(), structs.HealthCritical) {
		t.Fatalf("expected critical state error, got: %#v", err)
	}

	// Registering with a healthy check succeeds
	testRegisterCheck(t, s, 4, "node1", "", "check1", structs.HealthPassing)
	if err := s.SessionCreate(5, sess); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()

	// Check mappings were inserted
	check, err := tx.First("session_checks", "session", "bar")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if check == nil {
		t.Fatalf("missing session check")
	}
	expectCheck := &sessionCheck{
		Node:    "node1",
		CheckID: "check1",
		Session: "bar",
	}
	if actual := check.(*sessionCheck); !reflect.DeepEqual(actual, expectCheck) {
		t.Fatalf("expected %#v, got: %#v", expectCheck, actual)
	}
}

func TestStateStore_SessionList(t *testing.T) {
	s := testStateStore(t)

	// Listing when no sessions exist returns nil
	idx, res, err := s.SessionList()
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Register some nodes
	testRegisterNode(t, s, 1, "node1")
	testRegisterNode(t, s, 2, "node2")
	testRegisterNode(t, s, 3, "node3")

	// Create some sessions in the state store
	sessions := structs.Sessions{
		&structs.Session{
			ID:       "session1",
			Node:     "node1",
			Behavior: structs.SessionKeysDelete,
		},
		&structs.Session{
			ID:       "session2",
			Node:     "node2",
			Behavior: structs.SessionKeysRelease,
		},
		&structs.Session{
			ID:       "session3",
			Node:     "node3",
			Behavior: structs.SessionKeysDelete,
		},
	}
	for i, session := range sessions {
		if err := s.SessionCreate(uint64(4+i), session); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// List out all of the sessions
	idx, sessionList, err := s.SessionList()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 6 {
		t.Fatalf("bad index: %d", idx)
	}
	if !reflect.DeepEqual(sessionList, sessions) {
		t.Fatalf("bad: %#v", sessions)
	}
}

func TestStateStore_NodeSessions(t *testing.T) {
	s := testStateStore(t)

	// Listing sessions with no results returns nil
	idx, res, err := s.NodeSessions("node1")
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Create the nodes
	testRegisterNode(t, s, 1, "node1")
	testRegisterNode(t, s, 2, "node2")

	// Register some sessions with the nodes
	sessions1 := structs.Sessions{
		&structs.Session{
			ID:   "session1",
			Node: "node1",
		},
		&structs.Session{
			ID:   "session2",
			Node: "node1",
		},
	}
	sessions2 := []*structs.Session{
		&structs.Session{
			ID:   "session3",
			Node: "node2",
		},
		&structs.Session{
			ID:   "session4",
			Node: "node2",
		},
	}
	for i, sess := range append(sessions1, sessions2...) {
		if err := s.SessionCreate(uint64(3+i), sess); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// Query all of the sessions associated with a specific
	// node in the state store.
	idx, res, err = s.NodeSessions("node1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that the index was properly filtered based
	// on the provided node ID.
	if idx != 4 {
		t.Fatalf("bad index: %s", err)
	}

	// Check that the returned sessions match.
	if !reflect.DeepEqual(res, sessions1) {
		t.Fatalf("bad: %#v", res)
	}
}

func TestStateStore_SessionDestroy(t *testing.T) {
	s := testStateStore(t)

	// Session destroy is idempotent and returns no error
	// if the session doesn't exist.
	if err := s.SessionDestroy(1, "nope"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Ensure the index was not updated if nothing was destroyed
	if idx := s.maxIndex("sessions"); idx != 0 {
		t.Fatalf("bad index: %d", idx)
	}

	// Register a node
	testRegisterNode(t, s, 1, "node1")

	// Register a new session
	sess := &structs.Session{
		ID:   "session1",
		Node: "node1",
	}
	if err := s.SessionCreate(2, sess); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Destroy the session
	if err := s.SessionDestroy(3, "session1"); err != nil {
		t.Fatalf("err: %s", err)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()

	// Make sure the session is really gone
	sessions, err := tx.Get("sessions", "id")
	if err != nil || sessions.Next() != nil {
		t.Fatalf("session should not exist")
	}

	// Check that the index was updated
	if idx := s.maxIndex("sessions"); idx != 3 {
		t.Fatalf("bad index: %d", idx)
	}
}

func TestStateStore_ACLSet_ACLGet(t *testing.T) {
	s := testStateStore(t)

	// Querying ACL's with no results returns nil
	res, err := s.ACLGet("nope")
	if res != nil || err != nil {
		t.Fatalf("expected (nil, nil), got: (%#v, %#v)", res, err)
	}

	// Inserting an ACL with empty ID is disallowed
	if err := s.ACLSet(1, &structs.ACL{}); err == nil {
		t.Fatalf("expected %#v, got: %#v", ErrMissingACLID, err)
	}

	// Index is not updated if nothing is saved
	if idx := s.maxIndex("acls"); idx != 0 {
		t.Fatalf("bad index: %d", idx)
	}

	// Inserting valid ACL works
	acl := &structs.ACL{
		ID:    "acl1",
		Name:  "First ACL",
		Type:  structs.ACLTypeClient,
		Rules: "rules1",
	}
	if err := s.ACLSet(1, acl); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that the index was updated
	if idx := s.maxIndex("acls"); idx != 1 {
		t.Fatalf("err: %s", err)
	}

	// Retrieve the ACL again
	result, err := s.ACLGet("acl1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Check that the ACL matches the result
	expect := &structs.ACL{
		ID:    "acl1",
		Name:  "First ACL",
		Type:  structs.ACLTypeClient,
		Rules: "rules1",
		RaftIndex: structs.RaftIndex{
			CreateIndex: 1,
			ModifyIndex: 1,
		},
	}
	if !reflect.DeepEqual(result, expect) {
		t.Fatalf("bad: %#v", result)
	}

	// Update the ACL
	acl = &structs.ACL{
		ID:    "acl1",
		Name:  "First ACL",
		Type:  structs.ACLTypeClient,
		Rules: "rules2",
	}
	if err := s.ACLSet(2, acl); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Index was updated
	if idx := s.maxIndex("acls"); idx != 2 {
		t.Fatalf("bad: %d", idx)
	}

	// ACL was updated and matches expected value
	expect = &structs.ACL{
		ID:    "acl1",
		Name:  "First ACL",
		Type:  structs.ACLTypeClient,
		Rules: "rules2",
		RaftIndex: structs.RaftIndex{
			CreateIndex: 1,
			ModifyIndex: 2,
		},
	}
	if !reflect.DeepEqual(acl, expect) {
		t.Fatalf("bad: %#v", acl)
	}
}

func TestStateStore_ACLList(t *testing.T) {
	s := testStateStore(t)

	// Listing when no ACLs exist returns nil
	idx, res, err := s.ACLList()
	if idx != 0 || res != nil || err != nil {
		t.Fatalf("expected (0, nil, nil), got: (%d, %#v, %#v)", idx, res, err)
	}

	// Insert some ACLs
	acls := structs.ACLs{
		&structs.ACL{
			ID:    "acl1",
			Type:  structs.ACLTypeClient,
			Rules: "rules1",
			RaftIndex: structs.RaftIndex{
				CreateIndex: 1,
				ModifyIndex: 1,
			},
		},
		&structs.ACL{
			ID:    "acl2",
			Type:  structs.ACLTypeClient,
			Rules: "rules2",
			RaftIndex: structs.RaftIndex{
				CreateIndex: 2,
				ModifyIndex: 2,
			},
		},
	}
	for _, acl := range acls {
		if err := s.ACLSet(acl.ModifyIndex, acl); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// Query the ACLs
	idx, res, err = s.ACLList()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}

	// Check that the result matches
	if !reflect.DeepEqual(res, acls) {
		t.Fatalf("bad: %#v", res)
	}
}

func TestStateStore_ACL_Snapshot_Restore(t *testing.T) {
	s := testStateStore(t)

	// Insert some ACLs.
	acls := structs.ACLs{
		&structs.ACL{
			ID:    "acl1",
			Type:  structs.ACLTypeClient,
			Rules: "rules1",
			RaftIndex: structs.RaftIndex{
				CreateIndex: 1,
				ModifyIndex: 1,
			},
		},
		&structs.ACL{
			ID:    "acl2",
			Type:  structs.ACLTypeClient,
			Rules: "rules2",
			RaftIndex: structs.RaftIndex{
				CreateIndex: 2,
				ModifyIndex: 2,
			},
		},
	}
	for _, acl := range acls {
		if err := s.ACLSet(acl.ModifyIndex, acl); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// Snapshot the ACLs.
	snap := s.Snapshot()
	defer snap.Close()

	// Verify the snapshot.
	if idx := snap.LastIndex(); idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}
	dump, err := snap.ACLDump()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if !reflect.DeepEqual(dump, acls) {
		t.Fatalf("bad: %#v", dump)
	}

	// Restore the values into a new state store.
	func() {
		s := testStateStore(t)
		for _, acl := range dump {
			if err := s.ACLRestore(acl); err != nil {
				t.Fatalf("err: %s", err)
			}
		}

		// Read the restored ACLs back out and verify that they match.
		idx, res, err := s.ACLList()
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if idx != 2 {
			t.Fatalf("bad index: %d", idx)
		}
		if !reflect.DeepEqual(res, acls) {
			t.Fatalf("bad: %#v", res)
		}
	}()
}

func TestStateStore_ACLDelete(t *testing.T) {
	s := testStateStore(t)

	// Calling delete on an ACL which doesn't exist returns nil
	if err := s.ACLDelete(1, "nope"); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Index isn't updated if nothing is deleted
	if idx := s.maxIndex("acls"); idx != 0 {
		t.Fatalf("bad index: %d", idx)
	}

	// Insert an ACL
	if err := s.ACLSet(1, &structs.ACL{ID: "acl1"}); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Delete the ACL and check that the index was updated
	if err := s.ACLDelete(2, "acl1"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if idx := s.maxIndex("acls"); idx != 2 {
		t.Fatalf("bad index: %d", idx)
	}

	tx := s.db.Txn(false)
	defer tx.Abort()

	// Check that the ACL was really deleted
	result, err := tx.First("acls", "id", "acl1")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got: %#v", result)
	}
}

func TestStateStore_ACL_Watches(t *testing.T) {
	s := testStateStore(t)

	// Call functions that update the ACLs table and make sure a watch fires
	// each time.
	verifyWatch(t, s.GetTableWatch("acls"), func() {
		if err := s.ACLSet(1, &structs.ACL{ID: "acl1"}); err != nil {
			t.Fatalf("err: %s", err)
		}
	})
	verifyWatch(t, s.GetTableWatch("acls"), func() {
		if err := s.ACLDelete(2, "acl1"); err != nil {
			t.Fatalf("err: %s", err)
		}
	})
	verifyWatch(t, s.GetTableWatch("acls"), func() {
		if err := s.ACLRestore(&structs.ACL{ID: "acl1"}); err != nil {
			t.Fatalf("err: %s", err)
		}
	})
}
