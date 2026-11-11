mock_provider "aws" {
  mock_data "aws_ami" {
    defaults = { id = "ami-00000000000000001" }
  }
}

run "private_disposable_nodes" {
  command = plan
  module { source = "../../modules/acceptance" }
  variables {
    enabled                    = true
    name                       = "miles-fixture"
    region                     = "ap-southeast-1"
    account_id                 = "123456789012"
    ami_id                     = "ami-00000000000000001"
    instance_type              = "t4g.small"
    subnet_id                  = "subnet-00000000000000001"
    vpc_id                     = "vpc-00000000000000001"
    endpoint_security_group_id = "sg-00000000000000001"
    alb_security_group_id      = "sg-00000000000000002"
    permissions_boundary_arn   = "arn:aws:iam::123456789012:policy/boundary"
    kms_key_arn                = "arn:aws:kms:ap-southeast-1:123456789012:key/test"
    data_bucket_name           = "miles-fixture"
    owner_ssh_public_key       = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixture"
  }
  assert {
    condition = length(aws_instance.node) == 2 && alltrue([for node in aws_instance.node :
      !node.associate_public_ip_address && node.metadata_options[0].http_tokens == "required" &&
      node.credit_specification[0].cpu_credits == "standard" && node.root_block_device[0].encrypted &&
      node.root_block_device[0].delete_on_termination && node.root_block_device[0].volume_size == 20
    ])
    error_message = "Both acceptance nodes must be private and have encrypted disposable disks and IMDSv2."
  }
  assert {
    condition = alltrue([for name, role in aws_iam_role_policy.node : alltrue([
      for s in jsondecode(role.policy).Statement : !can(regex("dynamodb|secretsmanager|PutObject", jsonencode(s)))
    ])])
    error_message = "Bootstrap identities must not inherit runtime backend mutation or secret access."
  }
  assert {
    condition = alltrue([for name, role in aws_iam_role_policy.node : anytrue([
      for s in jsondecode(role.policy).Statement : s.Action == "s3:GetObjectVersion" && s.Resource == "arn:aws:s3:::miles-fixture/acceptance-assets/${name}/*"
    ])])
    error_message = "Each host must read only its own bootstrap assets."
  }
}
run "disabled_has_no_resources" {
  command = plan
  module { source = "../../modules/acceptance" }
  variables {
    enabled                    = false
    name                       = "miles-fixture"
    region                     = "ap-southeast-1"
    account_id                 = "123456789012"
    ami_id                     = null
    instance_type              = null
    subnet_id                  = "subnet-00000000000000001"
    vpc_id                     = "vpc-00000000000000001"
    endpoint_security_group_id = null
    alb_security_group_id      = "sg-00000000000000002"
    permissions_boundary_arn   = null
    kms_key_arn                = "arn:aws:kms:ap-southeast-1:123456789012:key/test"
    data_bucket_name           = "miles-fixture"
  }
  assert {
    condition     = length(aws_instance.node) == 0 && length(aws_iam_role.node) == 0 && length(aws_security_group.node) == 0
    error_message = "Disabling acceptance must remove the temporary nodes and identities."
  }
}
