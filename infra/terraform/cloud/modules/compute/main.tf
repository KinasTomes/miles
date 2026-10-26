# Pin an explicitly reviewed Amazon Linux 2023 ARM64 image, not a moving latest lookup.
data "aws_ami" "runtime" {
  count  = var.enabled && !var.paused ? 1 : 0
  owners = ["amazon"]
  filter {
    name   = "image-id"
    values = [var.ami_id]
  }
  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-arm64"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
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
resource "aws_ebs_volume" "data" {
  count             = var.enabled ? 1 : 0
  availability_zone = var.zone
  size              = 20
  type              = "gp3"
  encrypted         = true
  kms_key_id        = var.kms_key_arn
  tags              = { Name = "${var.name}-data", Purpose = "persistent-dev-data" }
  lifecycle { prevent_destroy = true }
}
resource "aws_instance" "runtime" {
  count                       = var.enabled && !var.paused ? 1 : 0
  ami                         = data.aws_ami.runtime[0].id
  instance_type               = "t4g.small"
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [var.security_group_id]
  associate_public_ip_address = false
  iam_instance_profile        = var.instance_profile
  monitoring                  = false
  volume_tags                 = { Project = "Miles", Environment = "dev", Name = "${var.name}-root" }
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }
  credit_specification { cpu_credits = "standard" }
  root_block_device {
    volume_type           = "gp3"
    volume_size           = 8
    encrypted             = true
    kms_key_id            = var.kms_key_arn
    delete_on_termination = true
  }
  # No app/secret/data formatting in cloud-init. Mount the retained volume before starting DSG.
  user_data                   = <<-SCRIPT
    #!/bin/bash
    set -euo pipefail
    systemctl enable --now amazon-ssm-agent
    install -d -m 0700 /var/lib/miles
  SCRIPT
  user_data_replace_on_change = true
  tags                        = { Name = "${var.name}-runtime" }
}
resource "aws_volume_attachment" "data" {
  count        = var.enabled && !var.paused ? 1 : 0
  device_name  = "/dev/sdf"
  volume_id    = aws_ebs_volume.data[0].id
  instance_id  = aws_instance.runtime[0].id
  force_detach = false
}
resource "aws_ssm_document" "shell" {
  count           = var.enabled ? 1 : 0
  name            = "${var.name}-shell"
  document_type   = "Session"
  document_format = "JSON"
  content = jsonencode({
    schemaVersion = "1.0"
    sessionType   = "Standard_Stream"
    description   = "Miles dev operator session; encrypted streaming audit logs"
    inputs = {
      kmsKeyId                    = var.kms_key_arn
      cloudWatchLogGroupName      = var.log_group_name
      cloudWatchEncryptionEnabled = true
      cloudWatchStreamingEnabled  = true
      idleSessionTimeout          = "20"
      maxSessionDuration          = "60"
      runAsEnabled                = false
    }
  })
}
output "inventory" {
  value = {
    instance_id      = try(aws_instance.runtime[0].id, null)
    data_volume_id   = try(aws_ebs_volume.data[0].id, null)
    session_document = try(aws_ssm_document.shell[0].name, null)
    instance_type    = "t4g.small"
    persistent_gib   = var.enabled ? 20 : 0
  }
}
