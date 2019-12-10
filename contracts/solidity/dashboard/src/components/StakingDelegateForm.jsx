import React, { Component } from 'react'
import PropTypes from 'prop-types'
import { Button, Form, FormGroup,
  FormControl } from 'react-bootstrap'
import WithWeb3Context from './WithWeb3Context'
import { formatAmount, displayAmount } from '../utils'

const ERRORS = {
  INVALID_AMOUNT: 'Invalid amount',
  SERVER: 'Sorry, your request cannot be completed at this time.'
}

const RESET_DELAY = 3000 // 3 seconds

class StakingDelegateForm extends Component {

  state = {
    amount: 100,
    operatorAddress: "",
    magpie: "",
    hasError: false,
    requestSent: false,
    requestSuccess: false,
    errorMsg: ERRORS.INVALID_AMOUNT
  }

  onChange = (e) => {
    const name = e.target.name
    this.setState(
      { [name]: e.target.value }
    )
  }

  onRequestSuccess() {
    this.setState({
      hasError: false,
      requestSent: true,
      requestSuccess: true
    })
    window.setTimeout(() => {
      this.setState(this.state)
    }, RESET_DELAY)
  }

  onClick = (e) => {
    this.submit()
  }

  onSubmit = (e) => {
    e.preventDefault()
  }

  onKeyUp = (e) => {
    if (e.keyCode === 13) {
      this.submit()
    }
  }

  validateAddress = (address) => {
    const { web3 } = this.props
    if (web3.utils && web3.utils.isAddress(address))
      return 'success'
    else
      return 'error'
  }

  validateAmount = () => {
    const { amount } = this.state
    const { web3, tokenBalance } = this.props
    if (web3.utils && tokenBalance && formatAmount(amount, 18).lte(tokenBalance))
      return 'success'
    else
      return 'error'
  }

  async submit() {
    const { amount, magpie, operatorAddress } = this.state
    const { web3 } = this.props;
    const stakingContractAddress = web3.stakingContract.options.address;
    let delegationData = '0x' + Buffer.concat([Buffer.from(magpie.substr(2), 'hex'), Buffer.from(operatorAddress.substr(2), 'hex')]).toString('hex');
    await web3.token.methods.approveAndCall(stakingContractAddress, web3.utils.toBN(formatAmount(amount, 18)).toString(), delegationData).send({from: web3.yourAddress})
  }

  render() {
    const { tokenBalance } = this.props
    const { amount, operatorAddress, magpie, hasError, errorMsg } = this.state
    
    let operatorAddressInfoStyle = {
      display: operatorAddress ? "block" : "none"
    }
    
    return (
      <Form onSubmit={this.onSubmit}>
        <h4>Operator address</h4>
        <FormGroup validationState={this.validateAddress(operatorAddress)}>
          <FormControl
            type="textarea"
            name="operatorAddress"
            value={operatorAddress}
            onChange={this.onChange}
          />
          <FormControl.Feedback />
        </FormGroup>
        <div style={operatorAddressInfoStyle} className="alert alert-info small">
          Operator: <strong>{operatorAddress}</strong>. Please check that the address is correct.
        </div>
        <h4>Magpie</h4>
        <p className="small">Address that receives earned rewards.</p>
        <FormGroup validationState={this.validateAddress(magpie)}>
          <FormControl
            type="text"
            name="magpie"
            value={magpie}
            onChange={this.onChange}
          />
          <FormControl.Feedback />
        </FormGroup>

        <p className="small"> You can stake up to { displayAmount(tokenBalance, 18, 3) } KEEP</p>

        <FormGroup validationState={this.validateAmount()}>
          <FormControl
            type="text"
            name="amount"
            value={amount}
            onChange={this.onChange}
            onKeyUp={this.onKeyUp}
          />
          <FormControl.Feedback />
        </FormGroup>

        <Button
          bsStyle="primary"
          bsSize="large"
          onClick={this.onClick}>
          Delegate stake
        </Button>
        { hasError &&
          <small className="error-message">{errorMsg}</small> }
      </Form>
    )
  }
}

StakingDelegateForm.propTypes = {
  btnText: PropTypes.string,
  action: PropTypes.string
}

export default WithWeb3Context(StakingDelegateForm);
