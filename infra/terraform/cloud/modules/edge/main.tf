locals {
  bootstrap = jsonencode({
    owner_ssh_public_key = var.owner_ssh_public_key
    account_id           = var.account_id
    region               = var.region
    bucket               = var.data_bucket_name
    release_key          = "releases/${var.release_sha256}/miles-linux-${var.architecture}.tar.gz"
    release_version      = var.release_version_id
    release_sha256       = var.release_sha256
    helper_key           = "deployment/helper/${var.helper_sha256}/aws_signing_helper"
    helper_version       = var.helper_version_id
    helper_sha256        = var.helper_sha256
    bootstrap_secret_arn = var.bootstrap_secret_arn
    runtime_secret_arn   = var.runtime_secret_arn
    kms_key_arn          = var.kms_key_arn
    table_prefix         = "${var.name}-"
    trust_anchor_arn     = var.provider_config.trust_anchor_arn
    profile_arn          = var.provider_config.profile_arn
    role_arn             = var.provider_config.role_arn
  })
}

data "aws_ami" "runtime" {
  count  = var.enabled ? 1 : 0
  owners = ["amazon"]
  filter {
    name   = "image-id"
    values = [var.ami_id]
  }
  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-${var.architecture == "amd64" ? "x86_64" : "arm64"}"]
  }
  filter {
    name   = "architecture"
    values = [var.architecture == "amd64" ? "x86_64" : "arm64"]
  }
  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
  filter {
    name   = "state"
    values = ["available"]
  }
}
resource "aws_instance" "runtime" {
  count                       = var.enabled ? 1 : 0
  ami                         = data.aws_ami.runtime[0].id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [var.security_group_id]
  associate_public_ip_address = true
  iam_instance_profile        = var.instance_profile
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }
  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    encrypted             = true
    kms_key_id            = var.kms_key_arn
    delete_on_termination = true
  }
  dynamic "credit_specification" {
    for_each = can(regex("^t[0-9]+[a-z]*\\.", var.instance_type)) ? [true] : []
    content { cpu_credits = "standard" }
  }
  user_data                   = templatefile("${path.module}/user-data.sh.tftpl", { bootstrap_json = local.bootstrap })
  user_data_replace_on_change = true
  tags                        = { Name = "${var.name}-edge" }
  volume_tags                 = { Project = "Miles", Installation = var.name, Environment = var.environment, Owner = var.owner, Name = "${var.name}-edge-root" }
}
output "inventory" {
  value = {
    configured           = var.enabled
    instance_id          = try(aws_instance.runtime[0].id, null)
    public_ip            = try(aws_instance.runtime[0].public_ip, null)
    private_ip           = try(aws_instance.runtime[0].private_ip, null)
    security_group_id    = var.security_group_id
    bootstrap_secret_arn = var.bootstrap_secret_arn
  }
}
