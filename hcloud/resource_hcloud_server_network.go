package hcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/hetznercloud/terraform-provider-hcloud/internal/merge"
)

func resourceServerNetwork() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceServerNetworkCreate,
		ReadContext:   resourceServerNetworkRead,
		UpdateContext: resourceServerNetworkUpdate,
		DeleteContext: resourceServerNetworkDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: map[string]*schema.Schema{
			"network_id": {
				Type:     schema.TypeInt,
				Optional: true,
				ForceNew: true,
			},
			"subnet_id": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},
			"server_id": {
				Type:     schema.TypeInt,
				Required: true,
				ForceNew: true,
			},
			"ip": {
				Type:     schema.TypeString,
				Computed: true,
				Optional: true,
				ForceNew: true,
			},
			"alias_ips": {
				Type:     schema.TypeSet,
				Elem:     &schema.Schema{Type: schema.TypeString},
				Optional: true,
			},
			"mac_address": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceServerNetworkCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	var aliasIPs []net.IP

	client := m.(*hcloud.Client)
	ip := net.ParseIP(d.Get("ip").(string))

	networkID, nwIDSet := d.GetOk("network_id")

	subNetID, snIDSet := d.GetOk("subnet_id")
	if (nwIDSet && snIDSet) || (!nwIDSet && !snIDSet) {
		return diag.Errorf("either network_id or subnet_id must be set")
	}
	if snIDSet {
		nwID, _, err := parseNetworkSubnetID(subNetID.(string))
		if err != nil {
			return diag.FromErr(err)
		}
		networkID = nwID
	}

	server := &hcloud.Server{ID: d.Get("server_id").(int)}
	network := &hcloud.Network{ID: networkID.(int)}

	for _, aliasIP := range d.Get("alias_ips").(*schema.Set).List() {
		ip := net.ParseIP(aliasIP.(string))
		aliasIPs = append(aliasIPs, ip)
	}

	err := attachServerToNetwork(ctx, client, server, network, ip, aliasIPs)
	if err != nil {
		return diag.FromErr(err)
	}
	d.SetId(generateServerNetworkID(server, network))

	return resourceServerNetworkRead(ctx, d, m)
}

func resourceServerNetworkUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*hcloud.Client)
	server, network, _, err := lookupServerNetworkID(ctx, d.Id(), client)
	if err == errInvalidServerNetworkID {
		log.Printf("[WARN] Invalid id (%s), removing from state: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}
	if server == nil {
		log.Printf("[WARN] Server (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}
	if network == nil {
		log.Printf("[WARN] Network (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}

	if d.HasChange("alias_ips") {
		opts := hcloud.ServerChangeAliasIPsOpts{
			Network: network,
		}
		for _, aliasIP := range d.Get("alias_ips").(*schema.Set).List() {
			ip := net.ParseIP(aliasIP.(string))
			opts.AliasIPs = append(opts.AliasIPs, ip)
		}
		action, _, err := client.Server.ChangeAliasIPs(ctx, server, opts)
		if err != nil {
			return diag.FromErr(err)
		}
		if err := waitForNetworkAction(ctx, client, action, network); err != nil {
			return diag.FromErr(err)
		}
	}
	return resourceServerNetworkRead(ctx, d, m)
}

func resourceServerNetworkRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*hcloud.Client)

	server, network, privateNet, err := lookupServerNetworkID(ctx, d.Id(), client)
	if err == errInvalidServerNetworkID {
		log.Printf("[WARN] Invalid id (%s), removing from state: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}
	if server == nil {
		log.Printf("[WARN] Server (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}
	if network == nil {
		log.Printf("[WARN] Network (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}
	if privateNet == nil {
		log.Printf("[WARN] Server Attachment (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}
	d.SetId(generateServerNetworkID(server, network))
	setServerNetworkSchema(d, server, network, privateNet)
	return nil

}

func resourceServerNetworkDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	var action *hcloud.Action

	client := m.(*hcloud.Client)

	server, network, _, err := lookupServerNetworkID(ctx, d.Id(), client)

	if err != nil {
		log.Printf("[WARN] Invalid id (%s), removing from state: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	err = retry(defaultMaxRetries, func() error {
		var err error

		action, _, err = client.Server.DetachFromNetwork(ctx, server, hcloud.ServerDetachFromNetworkOpts{
			Network: network,
		})
		if hcloud.IsError(err, hcloud.ErrorCodeConflict) || hcloud.IsError(err, hcloud.ErrorCodeLocked) {
			return err
		}
		return abortRetry(err)
	})

	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		// network has already been deleted
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}
	if err := waitForNetworkAction(ctx, client, action, network); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func setServerNetworkSchema(d *schema.ResourceData, server *hcloud.Server, network *hcloud.Network, serverPrivateNet *hcloud.ServerPrivateNet) {
	d.SetId(generateServerNetworkID(server, network))
	d.Set("ip", serverPrivateNet.IP.String())

	// We need to ensure that order of the list of alias_ips is kept stable no
	// matter what the Hetzner Cloud API returns. Therefore we merge the
	// returned IPs with the currently known alias_ips.
	tfAliasIPs := d.Get("alias_ips").(*schema.Set).List()
	aliasIPs := make([]string, len(tfAliasIPs))
	for i, v := range tfAliasIPs {
		aliasIPs[i] = v.(string)
	}
	hcAliasIPs := make([]string, len(serverPrivateNet.Aliases))
	for i, ip := range serverPrivateNet.Aliases {
		hcAliasIPs[i] = ip.String()
	}
	aliasIPs = merge.StringSlice(aliasIPs, hcAliasIPs)
	d.Set("alias_ips", aliasIPs)

	d.Set("mac_address", serverPrivateNet.MACAddress)
	if subnetID, ok := d.GetOk("subnet_id"); ok {
		d.Set("subnet_id", subnetID.(string))
	} else {
		d.Set("network_id", network.ID)
	}
	d.Set("server_id", server.ID)
}

func attachServerToNetwork(ctx context.Context, c *hcloud.Client, srv *hcloud.Server, nw *hcloud.Network, ip net.IP, aliasIPs []net.IP) error {
	var a *hcloud.Action

	opts := hcloud.ServerAttachToNetworkOpts{
		Network:  nw,
		IP:       ip,
		AliasIPs: aliasIPs,
	}

	err := retry(defaultMaxRetries, func() error {
		var err error

		a, _, err = c.Server.AttachToNetwork(ctx, srv, opts)
		if hcloud.IsError(err, hcloud.ErrorCodeConflict) || hcloud.IsError(err, hcloud.ErrorCodeLocked) {
			return err
		}
		if err != nil {
			return abortRetry(err)
		}
		return nil
	})
	if hcloud.IsError(err, hcloud.ErrorCodeServerAlreadyAttached) {
		log.Printf("[INFO] Server (%v) already attachted to network %v", srv.ID, nw.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("attach server to network: %v", err)
	}
	if err := waitForNetworkAction(ctx, c, a, nw); err != nil {
		return fmt.Errorf("attach server to network: %v", err)
	}
	return nil
}

func generateServerNetworkID(server *hcloud.Server, network *hcloud.Network) string {
	return fmt.Sprintf("%d-%d", server.ID, network.ID)
}

var errInvalidServerNetworkID = errors.New("invalid server network id")

// lookupServerNetworkID parses the terraform server network record id and return the server, network and the ServerPrivateNet
//
// id format: <server id>-<network id>
// Examples:
// 123-456
func lookupServerNetworkID(ctx context.Context, terraformID string, client *hcloud.Client) (server *hcloud.Server, network *hcloud.Network, serverPrivateNet *hcloud.ServerPrivateNet, err error) {
	if terraformID == "" {
		err = errInvalidServerNetworkID
		return
	}
	parts := strings.SplitN(terraformID, "-", 2)
	if len(parts) != 2 {
		err = errInvalidServerNetworkID
		return
	}

	serverID, err := strconv.Atoi(parts[0])
	if err != nil {
		err = errInvalidServerNetworkID
		return
	}

	server, _, err = client.Server.GetByID(ctx, serverID)
	if err != nil {
		err = errInvalidServerNetworkID
		return
	}
	if server == nil {
		err = errInvalidServerNetworkID
		return
	}

	networkID, err := strconv.Atoi(parts[1])
	if err != nil {
		err = errInvalidServerNetworkID
		return
	}

	network, _, err = client.Network.GetByID(ctx, networkID)
	if network == nil {
		err = errInvalidServerNetworkID
		return
	}

	for _, pn := range server.PrivateNet {
		if pn.Network.ID == network.ID {
			serverPrivateNet = &pn
			return
		}
	}
	return
}
