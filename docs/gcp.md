# Deploying Queueframe on Google Cloud

This guide creates a small, single-instance Queueframe deployment on Compute
Engine. It deliberately uses an isolated `gcloud` configuration, a dedicated
VPC, a reserved regional IPv4 address, IAP-only SSH, and no VM service account.
The examples use Mumbai (`asia-south1`); choose a region near the broker's API
when a different location is more appropriate.

The commands create billable resources. A Cloud Billing budget sends alerts;
it is not a spending cap and does not stop resources automatically. Review the
current [Compute Engine pricing](https://cloud.google.com/compute/vm-instance-pricing),
[disk pricing](https://cloud.google.com/compute/disks-image-pricing), and
[external IPv4 pricing](https://cloud.google.com/vpc/pricing) before proceeding.

## Choose deployment values

Use your own project ID and hostname. Do not paste Kite credentials, the
Queueframe master key, or the administrator password into these variables.

```sh
QF_GCP_PROJECT_ID="your-project-id"
QF_GCP_CONFIG="queueframe-gcp"
QF_GCP_REGION="asia-south1"
QF_GCP_ZONE="asia-south1-a"
QF_GCP_VM="queueframe"
QF_GCP_NETWORK="queueframe-net"
QF_GCP_SUBNET="queueframe-mumbai"
QF_GCP_ADDRESS="queueframe-ip"
QF_GCP_HOSTNAME="queueframe.example.com"
QF_GCP_MONTHLY_BUDGET="2000"
```

`e2-small` (shared CPU, 2 GB memory) is a comfortable default for a household
instance. Queueframe itself needs little CPU; stable egress, low network jitter,
clock synchronization, and a local persistent disk matter more. Confirm that
the selected machine type is offered in the chosen zone.

This guide assumes the project already exists, billing is linked, and your
Google identity may administer Compute Engine, IAP, networking, and billing
budgets for it. Create or link those in the Cloud Console first if necessary.

## Authenticate in an isolated CLI configuration

Creating a separate configuration avoids accidentally mutating another active
project. If that configuration name already exists, activate it instead of
creating it again.

```sh
gcloud config configurations create "$QF_GCP_CONFIG"
gcloud config configurations activate "$QF_GCP_CONFIG"
gcloud auth login
gcloud config set project "$QF_GCP_PROJECT_ID"
gcloud config set compute/region "$QF_GCP_REGION"
gcloud config set compute/zone "$QF_GCP_ZONE"

gcloud auth list
gcloud config list
```

Before every create or delete operation, verify that `core.project` is the
intended project. Inventory existing resources before reusing any name:

```sh
gcloud compute instances list
gcloud compute addresses list
gcloud compute firewall-rules list
gcloud compute networks list
```

## Create a monthly alert budget

The billing API may take a minute to become available after it is enabled.
The project's billing account must permit you to create budgets.

```sh
gcloud services enable billingbudgets.googleapis.com

QF_GCP_BILLING_ACCOUNT="$(gcloud billing projects describe "$QF_GCP_PROJECT_ID" \
  --format='value(billingAccountName)')"

gcloud billing budgets create \
  --billing-account="$QF_GCP_BILLING_ACCOUNT" \
  --display-name=queueframe-monthly \
  --budget-amount="$QF_GCP_MONTHLY_BUDGET" \
  --filter-projects="projects/$QF_GCP_PROJECT_ID" \
  --calendar-period=month \
  --threshold-rule=percent=0.50 \
  --threshold-rule=percent=0.80 \
  --threshold-rule=percent=1.00 \
  --threshold-rule=percent=1.00,basis=forecasted-spend
```

Google documents the notification behavior and optional Pub/Sub automation in
[Cloud Billing budgets](https://docs.cloud.google.com/billing/docs/how-to/budgets).
Do not treat a successful budget command as a hard cost limit.

## Create the network, address, and VM

The web firewall rule below is for the `domain` and `public-ip` deployment
profiles. For a private Tailscale-only deployment, omit that rule and the
`queueframe-web` VM tag; no public inbound web port is required. In every mode,
the reserved address remains necessary as the stable egress identity submitted
to Zerodha.

```sh
gcloud services enable compute.googleapis.com iap.googleapis.com

gcloud compute networks create "$QF_GCP_NETWORK" \
  --subnet-mode=custom

gcloud compute networks subnets create "$QF_GCP_SUBNET" \
  --network="$QF_GCP_NETWORK" \
  --region="$QF_GCP_REGION" \
  --range=10.20.0.0/24

gcloud compute firewall-rules create queueframe-allow-web \
  --network="$QF_GCP_NETWORK" \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:80,tcp:443,udp:443 \
  --source-ranges=0.0.0.0/0 \
  --target-tags=queueframe-web

gcloud compute firewall-rules create queueframe-allow-iap-ssh \
  --network="$QF_GCP_NETWORK" \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:22 \
  --source-ranges=35.235.240.0/20 \
  --target-tags=queueframe-admin

gcloud compute addresses create "$QF_GCP_ADDRESS" \
  --region="$QF_GCP_REGION" \
  --network-tier=PREMIUM

QF_GCP_IPV4="$(gcloud compute addresses describe "$QF_GCP_ADDRESS" \
  --region="$QF_GCP_REGION" --format='value(address)')"
printf 'Reserved address: %s\n' "$QF_GCP_IPV4"

gcloud compute instances create "$QF_GCP_VM" \
  --zone="$QF_GCP_ZONE" \
  --machine-type=e2-small \
  --subnet="$QF_GCP_SUBNET" \
  --address="$QF_GCP_IPV4" \
  --network-tier=PREMIUM \
  --tags=queueframe-web,queueframe-admin \
  --image-family=debian-12 \
  --image-project=debian-cloud \
  --boot-disk-size=10GB \
  --boot-disk-type=pd-balanced \
  --metadata=enable-oslogin=TRUE \
  --no-service-account \
  --no-scopes \
  --deletion-protection \
  --shielded-secure-boot \
  --shielded-vtpm \
  --shielded-integrity-monitoring
```

Google documents `35.235.240.0/20` as the IPv4 source range for
[IAP TCP forwarding](https://docs.cloud.google.com/iap/docs/using-tcp-forwarding).
There is no internet-wide SSH rule and Queueframe's port `8080` is not exposed.

Verify the resolved resources instead of relying on shell history:

```sh
gcloud compute addresses describe "$QF_GCP_ADDRESS" \
  --region="$QF_GCP_REGION" \
  --format='yaml(address,status,users)'

gcloud compute instances describe "$QF_GCP_VM" \
  --zone="$QF_GCP_ZONE" \
  --format='yaml(name,status,machineType,networkInterfaces,shieldedInstanceConfig)'
```

The address should be `IN_USE` and identify the new VM in `users`. The VM's
primary external address is also its normal outbound one-to-one NAT identity,
so it can be used for the Kite static-IP whitelist. See Google Cloud's
[Public NAT behavior](https://docs.cloud.google.com/nat/docs/public-nat) and
[static address guide](https://docs.cloud.google.com/vpc/docs/reserve-static-external-ip-address).

## Configure browser access

For domain mode, create an `A` record from `QF_GCP_HOSTNAME` to `QF_GCP_IPV4`.
If Cloudflare hosts the zone, begin with **DNS only** so Queueframe's doctor can
verify the origin address and Caddy can complete ACME validation. Do not add an
`AAAA` record unless IPv6 ingress is intentionally configured.

```sh
dig +short "$QF_GCP_HOSTNAME" A
```

Wait until public resolvers return the reserved address. The exact Kite app
redirect URL will be:

```text
https://queueframe.example.com/auth/kite/callback
```

Substitute the hostname selected above. Whitelist the confirmed reserved IPv4
only after it is attached and verified; Zerodha limits how frequently that
value may be changed.

Without a domain, use the Tailscale or numeric-IP profile described in the
[deployment guide](deployment.md). The stable GCP egress address is required in
all three modes.

## Install and start Queueframe

Connect through IAP:

```sh
gcloud compute ssh "$QF_GCP_VM" \
  --project="$QF_GCP_PROJECT_ID" \
  --zone="$QF_GCP_ZONE" \
  --tunnel-through-iap
```

On the VM:

1. Install Docker Engine and the Compose plugin using Docker's current
   [Debian instructions](https://docs.docker.com/engine/install/debian/).
2. Clone `https://github.com/abinthomasonline/queueframe.git` (authenticate to
   GitHub first while the repository is private).
3. Run `./deploy/setup.sh` and select the intended access mode.
4. Enter the verified egress address and a unique bootstrap administrator
   password. Keep the generated `deploy/.env` private.
5. Start and verify the stack:

```sh
./deploy/doctor.sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --build -d
./deploy/doctor.sh --running
```

Complete the first administrator sign-in, then remove bootstrap secrets from
the environment and stored container metadata:

```sh
./deploy/seal.sh
```

Keep `QUEUEFRAME_DRY_RUN=true` through onboarding and at least one complete
rehearsal. Follow [security and invitation guidance](security.md) before adding
a trader and [operations](operations.md) before enabling live placement.

## Acceptance checklist

- The reserved IPv4 is static, attached, and `IN_USE`.
- The monthly budget and its notification thresholds are visible in Billing.
- The selected HTTPS origin has a trusted certificate and `/healthz` responds.
- `./deploy/doctor.sh --running` passes and reports the configured egress.
- Host NTP is synchronized (`timedatectl status`).
- Internet clients cannot connect to host port `8080` or port `22`; IAP SSH
  still works.
- The Kite callback exactly matches the callback displayed by Queueframe.
- The administrator can create and revoke a single-use invitation.
- `./deploy/seal.sh` leaves the master key intact and removes bootstrap values.
- A permitted trader completes onboarding and a full dry-run rehearsal.

`/healthz` proves only that the web process responds. It does not prove Kite
authentication, the static-IP whitelist, scheduler readiness, or exchange
connectivity.

## Costs and teardown

VM runtime, its persistent disk, and an external IPv4 address can all incur
charges. Stopping a VM does not remove its disk or release its address. Use the
[Google Cloud Pricing Calculator](https://cloud.google.com/products/calculator)
with the selected region and current catalog prices rather than relying on a
dated estimate in this repository.

Before removing a deployment, take and test a backup as described in
[operations](operations.md). Resolve exact resource names with read-only
`describe` commands before deletion. Releasing the static address is
irreversible and can also force a Kite whitelist and callback change.
