locals {
  name             = "miles-dev"
  data_bucket_name = "miles-dev-data-${var.account_id}-${var.region}"
  egress_mode      = var.paused ? "isolated" : var.egress_mode
}
module "network" {
  source          = "../../modules/network"
  name            = local.name
  cidr            = var.vpc_cidr
  zones           = var.availability_zones
  egress_mode     = local.egress_mode
  nat_per_az      = var.nat_per_az
  client_cidrs    = var.client_cidrs
  admin_ssh_cidrs = var.admin_ssh_cidrs
}
module "endpoints" {
  source            = "../../modules/endpoints"
  name              = local.name
  region            = var.region
  vpc_id            = module.network.vpc_id
  private_subnets   = module.network.private_subnets
  route_table_ids   = module.network.private_route_table_ids
  gateway_sg_id     = module.network.security_groups.gateway
  jobs_sg_id        = module.network.security_groups.jobs
  enable_interfaces = local.egress_mode == "interface"
  data_bucket_name  = local.data_bucket_name
  table_arns        = module.data.table_arns
}
module "security" {
  source               = "../../modules/security"
  name                 = local.name
  region               = var.region
  account_id           = var.account_id
  autoscaling_role_arn = try(aws_iam_service_linked_role.autoscaling[0].arn, null)
}
module "data" {
  source      = "../../modules/data"
  name        = local.name
  bucket_name = local.data_bucket_name
  kms_key_arn = module.security.kms_key_arn
}
module "iam" {
  source                   = "../../modules/iam"
  name                     = local.name
  region                   = var.region
  account_id               = var.account_id
  data_bucket_name         = local.data_bucket_name
  table_arns               = module.data.table_arns
  kms_key_arn              = module.security.kms_key_arn
  secret_arn               = module.security.secret_arn
  parameter_arn            = module.security.parameter_arn
  log_group_arn            = module.security.log_group_arn
  permissions_boundary_arn = var.runtime_permissions_boundary_arn
  fleet_enabled            = var.enable_asg
  release_sha256           = var.release_sha256
}
module "compute" {
  source            = "../../modules/compute"
  name              = local.name
  enabled           = var.enable_compute
  paused            = var.paused
  ami_id            = var.ami_id
  zone              = var.availability_zones[0]
  subnet_id         = module.network.private_subnets[var.availability_zones[0]]
  security_group_id = module.network.security_groups.gateway
  instance_profile  = module.iam.instance_profile_names.gateway
  kms_key_arn       = module.security.kms_key_arn
  log_group_name    = module.security.log_group_name
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
}
