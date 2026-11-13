mock_provider "aws" {
  mock_data "aws_ami" { defaults = { id = "ami-0123456789abcdef0" } }
}
variables {
  enabled              = true
  name                 = "miles-member-test"
  region               = "ap-southeast-2"
  account_id           = "123456789012"
  environment          = "test"
  owner                = "member"
  ami_id               = "ami-0123456789abcdef0"
  architecture         = "arm64"
  instance_type        = "t4g.small"
  subnet_id            = "subnet-0123456789abcdef0"
  security_group_id    = "sg-0123456789abcdef0"
  instance_profile     = "miles-member-test-edge-bootstrap"
  kms_key_arn          = "arn:aws:kms:ap-southeast-2:123456789012:key/00000000-0000-0000-0000-000000000001"
  data_bucket_name     = "miles-member-test-data-123456789012-ap-southeast-2"
  release_sha256       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  release_version_id   = "version-one"
  runtime_secret_arn   = "arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:runtime-fixture"
  bootstrap_secret_arn = "arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:edge-fixture"
  helper_sha256        = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  helper_version_id    = "helper-version-one"
  provider_config = {
    trust_anchor_arn = "arn:aws:rolesanywhere:ap-southeast-2:123456789012:trust-anchor/00000000-0000-0000-0000-000000000001"
    profile_arn      = "arn:aws:rolesanywhere:ap-southeast-2:123456789012:profile/00000000-0000-0000-0000-000000000002"
    role_arn         = "arn:aws:iam::123456789012:role/miles/member/miles-member-test-edge"
  }
  owner_ssh_public_key = null
}
run "edge_bootstrap_is_pinned_and_secret_free" {
  command = plan
  module { source = "../../modules/edge" }
  assert {
    condition     = aws_instance.runtime[0].associate_public_ip_address && aws_instance.runtime[0].metadata_options[0].http_tokens == "required" && aws_instance.runtime[0].credit_specification[0].cpu_credits == "standard" && aws_instance.runtime[0].root_block_device[0].encrypted && aws_instance.runtime[0].root_block_device[0].delete_on_termination
    error_message = "Edge uses a bounded public-subnet VM with IMDSv2, standard credits and encrypted disposable storage."
  }
  assert {
    condition     = strcontains(aws_instance.runtime[0].user_data, var.bootstrap_secret_arn) && strcontains(aws_instance.runtime[0].user_data, var.release_sha256) && strcontains(aws_instance.runtime[0].user_data, var.helper_sha256) && !strcontains(aws_instance.runtime[0].user_data, "PRIVATE KEY") && !strcontains(aws_instance.runtime[0].user_data, "BEGIN CERTIFICATE")
    error_message = "User-data may contain only public references and version/hash pins, not secret material."
  }
}
run "paused_edge_retains_no_vm" {
  command = plan
  module { source = "../../modules/edge" }
  variables { enabled = false }
  assert {
    condition     = length(aws_instance.runtime) == 0 && output.inventory.instance_id == null
    error_message = "Paused Edge must have no EC2 instance."
  }
}
