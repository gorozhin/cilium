// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright Authors of Cilium */

#include <bpf/ctx/skb.h>
#include "common.h"
#include "pktgen.h"

/* Enable code paths under test */
#define ENABLE_IPV4
#define ENABLE_NODEPORT
#define SERVICE_NO_BACKEND_RESPONSE
#define ENABLE_MASQUERADE_IPV4		1

#define CLIENT_IP		v4_ext_one
#define CLIENT_PORT		__bpf_htons(111)

#define FRONTEND_IP		v4_svc_two
#define FRONTEND_PORT		tcp_svc_one

#define BACKEND_IP		v4_pod_two
#define BACKEND_PORT		__bpf_htons(8080)

static volatile const __u8 *client_mac = mac_one;
/* this matches the default node_config.h: */
static volatile const __u8 lb_mac[ETH_ALEN] = { 0xce, 0x72, 0xa7, 0x03, 0x88, 0x56 };

#include <bpf_host.c>

ASSIGN_CONFIG(union v4addr, nat_ipv4_masquerade, { .be32 = FRONTEND_IP})

#include "lib/ipcache.h"
#include "lib/lb.h"

#define FROM_NETDEV	0
#define TO_NETDEV	1

struct {
	__uint(type, BPF_MAP_TYPE_PROG_ARRAY);
	__uint(key_size, sizeof(__u32));
	__uint(max_entries, 2);
	__array(values, int());
} entry_call_map __section(".maps") = {
	.values = {
		[FROM_NETDEV] = &cil_from_netdev,
		[TO_NETDEV] = &cil_to_netdev,
	},
};

/* Test that a SVC without backends returns a TCP RST or ICMP error */
PKTGEN("tc", "tc_nodeport_no_backend")
int nodeport_no_backend_pktgen(struct __ctx_buff *ctx)
{
	struct pktgen builder;
	struct tcphdr *l4;
	void *data;

	/* Init packet builder */
	pktgen__init(&builder, ctx);

	l4 = pktgen__push_ipv4_tcp_packet(&builder,
					  (__u8 *)client_mac, (__u8 *)lb_mac,
					  CLIENT_IP, FRONTEND_IP,
					  CLIENT_PORT, FRONTEND_PORT);
	if (!l4)
		return TEST_ERROR;

	data = pktgen__push_data(&builder, default_data, sizeof(default_data));
	if (!data)
		return TEST_ERROR;

	/* Calc lengths, set protocol fields and calc checksums */
	pktgen__finish(&builder);

	return 0;
}

SETUP("tc", "tc_nodeport_no_backend")
int nodeport_no_backend_setup(struct __ctx_buff *ctx)
{
	__u16 revnat_id = 1;

	lb_v4_add_service(FRONTEND_IP, FRONTEND_PORT, IPPROTO_TCP, 1, revnat_id);

	ipcache_v4_add_entry(BACKEND_IP, 0, 112233, 0, 0);

	/* Jump into the entrypoint */
	tail_call_static(ctx, entry_call_map, FROM_NETDEV);

	/* Fail if we didn't jump */
	return TEST_ERROR;
}

static __always_inline int
validate_icmp_reply(const struct __ctx_buff *ctx, __u32 retval)
{
	void *data, *data_end;
	__u32 *status_code;
	struct ethhdr *l2;
	struct iphdr *l3;
	struct icmphdr *l4;

	test_init();

	data = (void *)(long)ctx_data(ctx);
	data_end = (void *)(long)ctx->data_end;

	if (data + sizeof(__u32) > data_end)
		test_fatal("status code out of bounds");

	status_code = data;

	test_log("Status code: %d", *status_code);
	assert(*status_code == retval);

	l2 = data + sizeof(__u32);
	if ((void *)l2 + sizeof(struct ethhdr) > data_end)
		test_fatal("l2 header out of bounds");

	assert(memcmp(l2->h_dest, (__u8 *)client_mac, sizeof(lb_mac)) == 0);
	assert(memcmp(l2->h_source, (__u8 *)lb_mac, sizeof(lb_mac)) == 0);
	assert(l2->h_proto == __bpf_htons(ETH_P_IP));

	l3 = data + sizeof(__u32) + sizeof(struct ethhdr);
	if ((void *)l3 + sizeof(struct iphdr) > data_end)
		test_fatal("l3 header out of bounds");

	assert(l3->saddr == FRONTEND_IP);
	assert(l3->daddr == CLIENT_IP);

	assert(l3->ihl == 5);
	assert(l3->version == 4);
	assert(l3->tos == 0);
	assert(l3->ttl == 64);
	assert(l3->protocol == IPPROTO_ICMP);

	if (l3->check != bpf_htons(0x4b8e))
		test_fatal("L3 checksum is invalid: %x", bpf_htons(l3->check));

	l4 = data + sizeof(__u32) + sizeof(struct ethhdr) + sizeof(struct iphdr);
	if ((void *) l4 + sizeof(struct icmphdr) > data_end)
		test_fatal("l4 header out of bounds");

	assert(l4->type == ICMP_DEST_UNREACH);
	assert(l4->code == ICMP_PORT_UNREACH);

	/* reference checksum is calculated with wireshark by dumping the
	 * context with the runner option and importing the packet into
	 * wireshark
	 */
	assert(l4->checksum == bpf_htons(0x2c3f));

	test_finish();
}

CHECK("tc", "tc_nodeport_no_backend")
int nodeport_no_backend_check(__maybe_unused const struct __ctx_buff *ctx)
{
	return validate_icmp_reply(ctx, CTX_ACT_REDIRECT);
}

/* Test that the ICMP error message leaves the node */
PKTGEN("tc", "tc_nodeport_no_backend2_reply")
int nodeport_no_backend2_reply_pktgen(struct __ctx_buff *ctx)
{
	/* Start with the initial request, and let SETUP() below rebuild it. */
	return nodeport_no_backend_pktgen(ctx);
}

SETUP("tc", "tc_nodeport_no_backend2_reply")
int nodeport_no_backend2_reply_setup(struct __ctx_buff *ctx)
{
	if (__tail_no_service_ipv4(ctx))
		return TEST_ERROR;

	/* Jump into the entrypoint */
	tail_call_static(ctx, entry_call_map, TO_NETDEV);

	/* Fail if we didn't jump */
	return TEST_ERROR;
}

CHECK("tc", "tc_nodeport_no_backend2_reply")
int nodeport_no_backend2_reply_check(__maybe_unused const struct __ctx_buff *ctx)
{
	return validate_icmp_reply(ctx, CTX_ACT_OK);
}
