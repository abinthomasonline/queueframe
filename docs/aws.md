# Deploying Queueframe on AWS

Queueframe recommends AWS Asia Pacific (Mumbai), `ap-south-1`, as a practical
default cloud region. In a 2018 Kite Connect forum reply, Zerodha staff [pointed
Kite workloads toward AWS
Mumbai](https://kite.trade/forum/discussion/comment/15653/#Comment_15653), while
noting in the same reply that REST traffic is behind Cloudflare and regional
latency can therefore be negligible within India. Region selection does not
guarantee API latency, exchange receipt, queue position, or fills.

This guide uses one EC2 instance, an encrypted local EBS volume, a directly
attached Elastic IP, and AWS Systems Manager Session Manager for administration.
The default browser path is domain + Caddy. Queueframe's other access modes are
also supported.

The resources in this guide are billable. EC2, EBS storage, snapshots, data
transfer, and every public IPv4 address can incur charges. An AWS Budget sends
alerts but is not a spending cap. Review the current [EC2
pricing](https://aws.amazon.com/ec2/pricing/on-demand/), [EBS
pricing](https://aws.amazon.com/ebs/pricing/), and [public IPv4
pricing](https://aws.amazon.com/vpc/pricing/) before creating resources.

## Recommended topology

```text
browser ──HTTPS──> Elastic IP ──> Caddy ──> Queueframe
                                      private Docker network

Queueframe ──HTTPS──> Elastic IP egress ──> Kite Connect
operator ──IAM──> Systems Manager Session Manager ──> EC2
```

The attached Elastic IP is both the stable public ingress address and the
outbound address Kite sees. AWS documents that an internet gateway performs
the public IPv4 one-to-one translation for an instance; see [Internet gateways
and public IPv4
addresses](https://docs.aws.amazon.com/vpc/latest/userguide/VPC_Internet_Gateway.html).
Do not put a NAT gateway, load balancer, or proxy in the outbound path unless
you deliberately verify which static address reaches Kite.

## Choose the AWS resources

Use the `ap-south-1` Region. AWS lists that identifier as [Asia Pacific
(Mumbai)](https://docs.aws.amazon.com/global-infrastructure/latest/regions/aws-regions.html).
Create the following through the AWS console or equivalent infrastructure as
code:

- one dedicated VPC and public subnet with a route to an internet gateway;
- one `t3.small` EC2 instance as a comfortable household default;
- a current Ubuntu Server 24.04 LTS AMI;
- one encrypted 10 GiB `gp3` root volume with delete-on-termination disabled if
  that matches the operator's recovery policy;
- one directly attached Elastic IP in `ap-south-1`;
- termination protection and IMDSv2 required; and
- one dedicated EC2 IAM role that permits Systems Manager Session Manager.

Queueframe is small enough for a `t3.micro`, but the extra memory on
`t3.small` gives Docker, operating-system updates, and source builds more room.
The published container supports both `amd64` and `arm64`; `t4g.small` is also
valid when the selected OS image and operator tooling support Arm.

AWS recommends requiring IMDSv2. Because Queueframe runs in containers, set the
metadata response hop limit to `2` if container access to instance metadata is
ever needed; Queueframe itself does not require it. See [EC2 metadata
options](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-IMDS-new-instances.html).

### Security group

For domain or public-IP mode, allow only these inbound rules:

| Source | Protocol | Port | Purpose |
| --- | --- | --- | --- |
| `0.0.0.0/0` | TCP | 80 | ACME validation and HTTPS redirect |
| `0.0.0.0/0` | TCP | 443 | HTTPS |
| `0.0.0.0/0` | UDP | 443 | HTTP/3; optional |

Do not expose TCP `8080`. Do not open TCP `22` when Session Manager is
available. For Tailscale mode, omit all public inbound rules. Retain outbound
HTTPS and the normal operating-system update paths.

Security groups start with no inbound rules, and AWS documents their behavior
in [Security group
rules](https://docs.aws.amazon.com/vpc/latest/userguide/security-group-rules.html).

### Session Manager

Attach a dedicated instance profile with the permissions required by Session
Manager. AWS documents both the standard managed-node permissions and how to
build a narrower role in [Session Manager instance
permissions](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-getting-started-instance-profile.html).
Do not put long-lived AWS access keys on the VM.

After the instance becomes a managed node, connect without opening SSH:

```sh
QF_AWS_PROFILE="queueframe-aws"
QF_AWS_REGION="ap-south-1"
QF_AWS_INSTANCE_ID="i-REPLACE_ME"

aws sts get-caller-identity --profile "$QF_AWS_PROFILE"
aws ssm start-session \
  --target "$QF_AWS_INSTANCE_ID" \
  --region "$QF_AWS_REGION" \
  --profile "$QF_AWS_PROFILE"
```

The workstation needs AWS CLI v2 and the Session Manager plugin. See [Connect
with Session
Manager](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/connect-with-systems-manager-session-manager.html).

## Reserve and verify the Elastic IP

Allocate an Elastic IP in `ap-south-1` and associate it directly with the EC2
instance's primary network interface. It remains allocated to the account until
it is explicitly released, and AWS charges for public IPv4 addresses whether
they are attached or idle. See [Elastic IP
addresses](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/elastic-ip-addresses-eip.html).

From the instance, confirm the public egress address before entering it in
Queueframe or Zerodha:

```sh
curl --fail --silent --show-error --ipv4 https://checkip.amazonaws.com
```

That result must equal the attached Elastic IP and later
`QUEUEFRAME_EXPECTED_PUBLIC_IP`. Do not release the address while the instance
is in service: a replacement address requires DNS and Kite whitelist changes.

## Install Queueframe

Install Docker Engine and the Compose v2 plugin using Docker's current [Ubuntu
installation guide](https://docs.docker.com/engine/install/ubuntu/). Also
install Git and OpenSSL, enable Docker at boot, and retain the host's normal
clock-synchronization service.

Clone Queueframe and run the deployment setup:

```sh
git clone https://github.com/abinthomasonline/queueframe.git
cd queueframe
./deploy/setup.sh
./deploy/doctor.sh
```

Select `domain` for the normal public household deployment. Supply the Elastic
IP as both the expected egress and ingress address. Setup writes
`deploy/.env`, generates the encryption key, and leaves dry-run enabled.

### Use a released container image

Official release images are public and support `amd64` and `arm64`. Prefer an
immutable `VERSION@sha256:DIGEST` reference shown by the GitHub release/package
instead of a moving tag. Edit the generated `deploy/.env`:

```text
QUEUEFRAME_IMAGE=ghcr.io/abinthomasonline/queueframe:VERSION@sha256:DIGEST
```

Then pull and start without building on the VM:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml pull queueframe
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --no-build -d
./deploy/doctor.sh --running
```

No registry login is needed for the official public package. Building from the
checked-out source remains supported with `up --build -d`.

## DNS and Kite Connect

For domain mode:

1. Create a DNS-only `A` record from the chosen hostname to the Elastic IP.
2. Do not add an `AAAA` record unless IPv6 ingress is deliberately configured.
3. Wait until public DNS returns the Elastic IP.
4. Start the stack and verify browser-trusted HTTPS.
5. In every trader's Kite Connect app, whitelist the Elastic IP and register
   the exact callback:

   ```text
   https://queueframe.example.com/auth/kite/callback
   ```

Queueframe displays the configured address and callback during onboarding.
Review Zerodha's current [static-IP
rules](https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip)
before changing the whitelist.

## First-run checklist

1. Run `./deploy/doctor.sh --running` and verify the egress address, DNS, HTTPS,
   clock synchronization, and one healthy Queueframe container.
2. Sign in as the bootstrap administrator, then run `./deploy/seal.sh` to remove
   bootstrap credentials from the environment and container metadata.
3. Create an invitation and onboard a trader.
4. Complete the entire order, activation, scheduling, and receipt workflow with
   `QUEUEFRAME_DRY_RUN=true`.
5. Back up `deploy/.env`, Queueframe data, and ingress state as described in
   [operations](operations.md).
6. Enable live placement only after the dry-run result and Kite static-IP
   configuration have been verified.

Create an [AWS Budget](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-managing-costs.html)
with actual and forecast alerts. Treat it as notification only; AWS notes that
usage can continue beyond an alert threshold.

## Recovery and teardown

Use Session Manager for inspection and recovery. A stopped EBS snapshot is a
useful second layer after a verified application backup, not a substitute for
one. Keep the Elastic IP allocated while a recoverable deployment may return.

When permanently removing the instance, inventory and deliberately remove the
EC2 instance, EBS volumes and snapshots, Elastic IP, security group, IAM
instance profile/role, subnet, route table, internet gateway, and VPC. AWS
continues charging for retained resources, including an idle Elastic IP.
