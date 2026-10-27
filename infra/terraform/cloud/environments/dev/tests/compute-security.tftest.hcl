mock_provider "aws" {
  mock_data "aws_ami" { defaults = { id = "ami-0123456789abcdef0" } }
}
run "private_budgeted_node" {
  command = plan
  module { source = "../../modules/compute" }
  variables {
    name              = "miles-dev"
    enabled           = true
    paused            = false
    ami_id            = "ami-0123456789abcdef0"
    zone              = "ap-southeast-1a"
    subnet_id         = "subnet-0123456789abcdef0"
    security_group_id = "sg-0123456789abcdef0"
    instance_profile  = "miles-dev-gateway"
    kms_key_arn       = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001"
    log_group_name    = "/miles-dev/runtime"
  }
  assert {
    condition     = length(aws_instance.runtime) == 1 && !aws_instance.runtime[0].associate_public_ip_address && aws_instance.runtime[0].instance_type == "t4g.small" && aws_instance.runtime[0].metadata_options[0].http_tokens == "required" && aws_instance.runtime[0].credit_specification[0].cpu_credits == "standard" && aws_instance.runtime[0].volume_tags.Project == "Miles" && aws_instance.runtime[0].volume_tags.Environment == "dev"
    error_message = "One private ARM dev node must enforce IMDSv2 and standard CPU credits."
  }
  assert {
    condition     = aws_instance.runtime[0].root_block_device[0].encrypted && aws_instance.runtime[0].root_block_device[0].delete_on_termination && aws_ebs_volume.data[0].encrypted && aws_ebs_volume.data[0].size == 20 && !aws_volume_attachment.data[0].force_detach && jsondecode(aws_ssm_document.shell[0].content).inputs.cloudWatchEncryptionEnabled
    error_message = "Root is disposable; encrypted data volume and encrypted session audit must be retained safely."
  }
}
