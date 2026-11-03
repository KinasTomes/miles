locals {
  name             = var.installation_name
  zones            = coalesce(var.availability_zones, ["${var.region}a", "${var.region}b"])
  data_bucket_name = "${local.name}-data-${var.account_id}-${var.region}"
  egress_mode      = var.paused ? "isolated" : var.egress_mode
}
module "network" {
  source          = "../../modules/network"
  name            = local.name
  cidr            = var.vpc_cidr
  zones           = local.zones
  egress_mode     = local.egress_mode
  nat_per_az      = var.nat_per_az
  client_cidrs    = var.client_cidrs
  admin_ssh_cidrs = var.admin_ssh_cidrs
  enable_edge     = var.enable_edge
}
module "endpoints" {
  source                       = "../../modules/endpoints"
  name                         = local.name
  region                       = var.region
  vpc_id                       = module.network.vpc_id
  private_subnets              = module.network.private_subnets
  route_table_ids              = module.network.private_route_table_ids
  gateway_sg_id                = module.network.security_groups.gateway
  jobs_sg_id                   = module.network.security_groups.jobs
  enable_interfaces            = local.egress_mode == "interface"
  data_bucket_name             = local.data_bucket_name
  table_arns                   = module.data.table_arns
  enable_edge                  = var.enable_edge
  edge_sg_id                   = try(module.network.security_groups.edge, null)
  edge_trust_anchor_arn        = try(module.iam.edge_provider.trust_anchor_arn, null)
  edge_certificate_common_name = var.edge_certificate_common_name
}
module "security" {
  source               = "../../modules/security"
  name                 = local.name
  region               = var.region
  account_id           = var.account_id
  autoscaling_role_arn = try(aws_iam_service_linked_role.autoscaling[0].arn, null)
  runtime_secret_name  = var.runtime_secret_name
}
module "data" {
  source      = "../../modules/data"
  name        = local.name
  bucket_name = local.data_bucket_name
  kms_key_arn = module.security.kms_key_arn
  # Shared R1 fences and three node supervisors exceed the single-node dev ceiling.
  max_request_units = var.enable_asg ? 128 : 10
}
module "iam" {
  source                       = "../../modules/iam"
  name                         = local.name
  region                       = var.region
  account_id                   = var.account_id
  data_bucket_name             = local.data_bucket_name
  table_arns                   = module.data.table_arns
  kms_key_arn                  = module.security.kms_key_arn
  secret_arn                   = module.security.secret_arn
  parameter_arn                = module.security.parameter_arn
  log_group_arn                = module.security.log_group_arn
  permissions_boundary_arn     = var.runtime_permissions_boundary_arn
  runtime_iam_path             = var.runtime_iam_path
  edge_certificate_common_name = var.edge_certificate_common_name
  fleet_enabled                = var.enable_asg
  release_sha256               = var.release_sha256
  edge_ca_pem                  = var.edge_ca_pem
  enable_edge                  = var.enable_edge
  edge_bootstrap_secret_arn    = try(aws_secretsmanager_secret.edge_bootstrap[0].arn, null)
  edge_helper_sha256           = var.edge_helper_sha256
}
module "compute" {
  source            = "../../modules/compute"
  name              = local.name
  enabled           = var.enable_compute
  paused            = var.paused
  ami_id            = var.ami_id
  zone              = local.zones[0]
  subnet_id         = module.network.private_subnets[local.zones[0]]
  security_group_id = module.network.security_groups.gateway
  instance_profile  = module.iam.instance_profile_names.gateway
  kms_key_arn       = module.security.kms_key_arn
  log_group_name    = module.security.log_group_name
  environment       = var.environment
  owner             = var.owner
  instance_type     = var.instance_type
  architecture      = var.release_architecture
}
module "alb" {
  source            = "../../modules/alb"
  name              = local.name
  enabled           = !var.paused && var.enable_alb
  vpc_id            = module.network.vpc_id
  subnet_ids        = values(module.network.public_subnets)
  security_group_id = module.network.security_groups.alb
  certificate_arn   = var.certificate_arn
  client_cidrs      = var.client_cidrs
}

# The retained single-node data disk never belongs to an interchangeable fleet.
resource "aws_iam_service_linked_role" "autoscaling" {
  count            = var.enable_asg ? 1 : 0
  aws_service_name = "autoscaling.amazonaws.com"
  custom_suffix    = local.name
  description      = "Miles ASG role for encrypted disposable node disks"
}
module "fleet" {
  source                  = "../../modules/fleet"
  depends_on              = [module.alb, module.endpoints]
  enabled                 = var.enable_asg && !var.paused
  name                    = local.name
  account_id              = var.account_id
  region                  = var.region
  ami_id                  = var.ami_id
  instance_type           = var.fleet_instance_type
  architecture            = var.release_architecture
  subnet_ids              = values(module.network.private_subnets)
  security_group_id       = module.network.security_groups.gateway
  instance_profile        = module.iam.instance_profile_names.gateway
  service_linked_role_arn = try(aws_iam_service_linked_role.autoscaling[0].arn, null)
  kms_key_arn             = module.security.kms_key_arn
  target_group_arn        = module.alb.target_group_arn
  data_bucket_name        = local.data_bucket_name
  release_sha256          = var.release_sha256
  release_version_id      = var.release_version_id
  runtime_secret_arn      = module.security.secret_arn
  owner_ssh_public_key    = var.owner_ssh_public_key
  environment             = var.environment
  owner                   = var.owner
}

resource "aws_secretsmanager_secret" "edge_bootstrap" {
  count                   = var.edge_ca_pem != null ? 1 : 0
  name                    = "${local.name}/edge-bootstrap"
  kms_key_id              = module.security.kms_key_arn
  recovery_window_in_days = 30
}
module "edge" {
  count                = var.enable_edge ? 1 : 0
  source               = "../../modules/edge"
  depends_on           = [module.endpoints]
  enabled              = !var.paused
  name                 = local.name
  account_id           = var.account_id
  region               = var.region
  environment          = var.environment
  owner                = var.owner
  ami_id               = var.ami_id
  architecture         = var.release_architecture
  instance_type        = var.edge_instance_type
  subnet_id            = module.network.public_subnets[local.zones[0]]
  security_group_id    = module.network.security_groups.edge
  instance_profile     = module.iam.edge_bootstrap_profile
  data_bucket_name     = local.data_bucket_name
  kms_key_arn          = module.security.kms_key_arn
  runtime_secret_arn   = module.security.secret_arn
  bootstrap_secret_arn = aws_secretsmanager_secret.edge_bootstrap[0].arn
  release_sha256       = var.release_sha256
  release_version_id   = var.release_version_id
  helper_sha256        = var.edge_helper_sha256
  helper_version_id    = var.edge_helper_version_id
  provider_config      = module.iam.edge_provider
  owner_ssh_public_key = var.owner_ssh_public_key
}
