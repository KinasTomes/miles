locals {
  # Same shared metadata contract; IAM does not implement DSG user grants.
  table_actions = ["dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:BatchWriteItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:DescribeTable", "dynamodb:ConditionCheckItem"]
  roles         = toset(concat(["gateway", "jobs"], var.edge_ca_pem == null ? [] : ["edge"]))
  # recovery/active-history.json deliberately excluded: only offline provisioning/recovery may write it.
  writable_prefixes = ["packs/", "packs-index/", "journal/", "checkpoints/"]
}
resource "aws_iam_role" "runtime" {
  for_each             = local.roles
  name                 = "${var.name}-${each.key}"
  path                 = "/miles/dev/"
  permissions_boundary = var.permissions_boundary_arn
  assume_role_policy = each.key == "edge" ? jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow", Action = ["sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"],
      Principal = { Service = "rolesanywhere.amazonaws.com" },
      Condition = {
        ArnEquals    = { "aws:SourceArn" = aws_rolesanywhere_trust_anchor.edge[0].arn },
        StringEquals = { "aws:SourceAccount" = var.account_id, "aws:PrincipalTag/x509Subject/CN" = "miles-edge" }
      }
    }]
    }) : jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["sts:AssumeRole"], Principal = { Service = "ec2.amazonaws.com" } }]
  })
}
resource "aws_iam_instance_profile" "runtime" {
  for_each = toset(["gateway", "jobs"])
  name     = "${var.name}-${each.key}"
  path     = "/miles/dev/"
  role     = aws_iam_role.runtime[each.key].name
}
resource "aws_rolesanywhere_trust_anchor" "edge" {
  count   = var.edge_ca_pem == null ? 0 : 1
  name    = "${var.name}-edge"
  enabled = true
  source {
    source_type = "CERTIFICATE_BUNDLE"
    source_data { x509_certificate_data = var.edge_ca_pem }
  }
}
resource "aws_rolesanywhere_profile" "edge" {
  count            = var.edge_ca_pem == null ? 0 : 1
  name             = "${var.name}-edge"
  role_arns        = [aws_iam_role.runtime["edge"].arn]
  enabled          = true
  duration_seconds = 900
}
resource "aws_iam_role_policy" "runtime" {
  for_each = local.roles
  name     = "${var.name}-runtime"
  role     = aws_iam_role.runtime[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(each.key == "gateway" ? [
      { Sid = "RegisterDevNode", Effect = "Allow", Action = ["ssm:UpdateInstanceInformation"], Resource = "arn:aws:ec2:${var.region}:${var.account_id}:instance/*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } },
      { Sid = "AgentChannels", Effect = "Allow", Action = ["ssmmessages:CreateControlChannel", "ssmmessages:CreateDataChannel", "ssmmessages:OpenControlChannel", "ssmmessages:OpenDataChannel"], Resource = "*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } },
      { Sid = "VerifySessionLogEncryption", Effect = "Allow", Action = ["logs:DescribeLogGroups"], Resource = "*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } }
      ] : [], [
      { Sid = "ListData", Effect = "Allow", Action = ["s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"], Resource = "arn:aws:s3:::${var.data_bucket_name}" },
      { Sid = "ReadData", Effect = "Allow", Action = ["s3:GetObject"], Resource = "arn:aws:s3:::${var.data_bucket_name}/*" },
      { Sid = "WriteData", Effect = "Allow", Action = ["s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts", "s3:RestoreObject"], Resource = [for p in local.writable_prefixes : "arn:aws:s3:::${var.data_bucket_name}/${p}*"] },
      { Sid = "Metadata", Effect = "Allow", Action = local.table_actions, Resource = concat(var.table_arns, [for a in var.table_arns : "${a}/index/*"]) },
      { Sid = "RuntimeCrypto", Effect = "Allow", Action = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey", "kms:DescribeKey"], Resource = var.kms_key_arn },
      { Sid = "ReadBootstrap", Effect = "Allow", Action = ["ssm:GetParameter"], Resource = var.parameter_arn },
      { Sid = "ReadSecrets", Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = var.secret_arn },
      { Sid = "Logs", Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"], Resource = "${var.log_group_arn}:*" },
      { Sid = "Metrics", Effect = "Allow", Action = ["cloudwatch:PutMetricData"], Resource = "*", Condition = { StringEquals = { "cloudwatch:namespace" = "Miles/DSG" } } }
      ], [for statement in [
        { Sid = "ReadPinnedRelease", Effect = "Allow", Action = ["s3:GetObjectVersion"], Resource = "arn:aws:s3:::${var.data_bucket_name}/releases/${coalesce(var.release_sha256, "unconfigured")}/*" },
        { Sid = "DrainOwnFleetTargets", Effect = "Allow", Action = ["elasticloadbalancing:DeregisterTargets"], Resource = "arn:aws:elasticloadbalancing:${var.region}:${var.account_id}:targetgroup/${var.name}-s3/*" },
        { Sid = "ObserveFleetDrain", Effect = "Allow", Action = ["elasticloadbalancing:DescribeTargetHealth"], Resource = "*", Condition = { StringEquals = { "aws:RequestedRegion" = var.region } } },
        { Sid = "OwnFleetLifecycle", Effect = "Allow", Action = ["autoscaling:RecordLifecycleActionHeartbeat", "autoscaling:CompleteLifecycleAction"], Resource = "arn:aws:autoscaling:${var.region}:${var.account_id}:autoScalingGroup:*:autoScalingGroupName/${var.name}-gateway" }
        ] : statement if var.fleet_enabled && each.key == "gateway"], [for statement in [
        { Sid = "CleanupData", Effect = "Allow", Action = ["s3:DeleteObject"], Resource = [for p in local.writable_prefixes : "arn:aws:s3:::${var.data_bucket_name}/${p}*"] },
        { Sid = "DrainTargets", Effect = "Allow", Action = ["elasticloadbalancing:DeregisterTargets"], Resource = "arn:aws:elasticloadbalancing:${var.region}:${var.account_id}:targetgroup/${var.name}-s3/*" },
        { Sid = "ASGLifecycle", Effect = "Allow", Action = ["autoscaling:RecordLifecycleActionHeartbeat", "autoscaling:CompleteLifecycleAction"], Resource = "arn:aws:autoscaling:${var.region}:${var.account_id}:autoScalingGroup:*:autoScalingGroupName/${var.name}-*" }
    ] : statement if each.key == "jobs"])
  })
}
output "role_arns" { value = { for n, r in aws_iam_role.runtime : n => r.arn } }
output "instance_profile_names" {
  value      = { for n, p in aws_iam_instance_profile.runtime : n => p.name }
  depends_on = [aws_iam_role_policy.runtime]
}
output "edge_provider" {
  value = var.edge_ca_pem == null ? null : {
    trust_anchor_arn        = aws_rolesanywhere_trust_anchor.edge[0].arn
    profile_arn             = aws_rolesanywhere_profile.edge[0].arn
    role_arn                = aws_iam_role.runtime["edge"].arn
    certificate_common_name = "miles-edge"
    duration_seconds        = 900
  }
}
