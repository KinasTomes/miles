locals {
  asg_name    = "${var.name}-gateway"
  hook_name   = "${var.name}-drain"
  release_key = var.release_sha256 == null ? null : "releases/${var.release_sha256}/miles-linux-${var.architecture}.tar.gz"
  bootstrap = jsonencode({
    account_id       = var.account_id
    region           = var.region
    asg_name         = local.asg_name
    hook_name        = local.hook_name
    target_group_arn = var.target_group_arn
    bucket           = var.data_bucket_name
    release_key      = local.release_key
    release_sha256   = var.release_sha256
    release_version  = var.release_version_id
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

resource "aws_launch_template" "gateway" {
  count         = var.enabled ? 1 : 0
  name_prefix   = "${var.name}-gateway-"
  image_id      = data.aws_ami.runtime[0].id
  instance_type = var.instance_type
  # Both AZs use disposable node-local disks; authoritative metadata is shared.
  iam_instance_profile { name = var.instance_profile }
  network_interfaces {
    associate_public_ip_address = false
    security_groups             = [var.security_group_id]
    delete_on_termination       = true
  }
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1 # Native systemd services, not a container bridge.
    instance_metadata_tags      = "disabled"
  }
  block_device_mappings {
    device_name = data.aws_ami.runtime[0].root_device_name
    ebs {
      volume_type           = "gp3"
      volume_size           = 20
      encrypted             = true
      kms_key_id            = var.kms_key_arn
      delete_on_termination = true
    }
  }
  dynamic "credit_specification" {
    for_each = startswith(var.instance_type == null ? "" : var.instance_type, "t") ? [1] : []
    content { cpu_credits = "standard" }
  }
  monitoring { enabled = true }
  tags      = { Project = "Miles", Environment = "dev", Name = local.asg_name }
  user_data = base64encode(templatefile("${path.module}/user-data.sh.tftpl", { bootstrap_json = local.bootstrap }))
  tag_specifications {
    resource_type = "instance"
    tags          = { Project = "Miles", Environment = "dev", Name = local.asg_name }
  }
  tag_specifications {
    resource_type = "volume"
    tags          = { Project = "Miles", Environment = "dev", Name = "${var.name}-cache" }
  }
  lifecycle {
    create_before_destroy = true
    precondition {
      condition     = length(var.subnet_ids) == 2 && var.ami_id != null && var.instance_type != null && var.release_version_id != null && can(regex("^[a-f0-9]{64}$", var.release_sha256))
      error_message = "Fleet requires two reviewed subnets, pinned AMI/type, release SHA-256 and S3 version ID."
    }
  }
}

resource "aws_autoscaling_group" "gateway" {
  count                     = var.enabled ? 1 : 0
  name                      = local.asg_name
  min_size                  = 2
  max_size                  = 2
  desired_capacity          = 2
  vpc_zone_identifier       = var.subnet_ids
  target_group_arns         = [var.target_group_arn]
  service_linked_role_arn   = var.service_linked_role_arn
  health_check_type         = "ELB"
  health_check_grace_period = 600
  default_instance_warmup   = 300
  force_delete              = false
  wait_for_capacity_timeout = "15m"
  launch_template {
    id      = aws_launch_template.gateway[0].id
    version = tostring(aws_launch_template.gateway[0].latest_version)
  }
  initial_lifecycle_hook {
    name                 = local.hook_name
    lifecycle_transition = "autoscaling:EC2_INSTANCE_TERMINATING"
    heartbeat_timeout    = 300
    default_result       = "CONTINUE"
  }
  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 100
      max_healthy_percentage = 150
      instance_warmup        = 300
      skip_matching          = true
      auto_rollback          = true
    }
  }
  tag {
    key                 = "Project"
    value               = "Miles"
    propagate_at_launch = true
  }
  tag {
    key                 = "Environment"
    value               = "dev"
    propagate_at_launch = true
  }
}

output "contract" {
  value = var.enabled ? {
    asg_name       = aws_autoscaling_group.gateway[0].name
    min_size       = aws_autoscaling_group.gateway[0].min_size
    desired        = aws_autoscaling_group.gateway[0].desired_capacity
    max_size       = aws_autoscaling_group.gateway[0].max_size
    health_type    = aws_autoscaling_group.gateway[0].health_check_type
    subnets        = aws_autoscaling_group.gateway[0].vpc_zone_identifier
    hook_name      = local.hook_name
    heartbeat      = 300
    app_idle       = 150
    drain          = 1800
    controller_max = 2100
    release_sha256 = var.release_sha256
    release_key    = local.release_key
  } : null
}
